/*
   Copyright 2020 Docker Compose CLI authors

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at
       http://www.apache.org/licenses/LICENSE-2.0
   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package compose

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	moby "github.com/docker/docker/api/types"

	"github.com/docker/compose/v2/internal/sync"

	"github.com/compose-spec/compose-go/types"
	"github.com/jonboulle/clockwork"
	"github.com/mitchellh/mapstructure"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"

	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/watch"
)

type DevelopmentConfig struct {
	Watch []Trigger `json:"watch,omitempty"`
}

type WatchAction string

const (
	WatchActionSync    WatchAction = "sync"
	WatchActionRebuild WatchAction = "rebuild"
)

type Trigger struct {
	Path   string   `json:"path,omitempty"`
	Action string   `json:"action,omitempty"`
	Target string   `json:"target,omitempty"`
	Ignore []string `json:"ignore,omitempty"`
}

const quietPeriod = 500 * time.Millisecond

// fileEvent contains the Compose service and modified host system path.
type fileEvent struct {
	sync.PathMapping
	Action WatchAction
}

// getSyncImplementation returns the the tar-based syncer unless it has been explicitly
// disabled with `COMPOSE_EXPERIMENTAL_WATCH_TAR=0`. Note that the absence of the env
// var means enabled.
func (s *composeService) getSyncImplementation(project *types.Project) sync.Syncer {
	var useTar bool
	if useTarEnv, ok := os.LookupEnv("COMPOSE_EXPERIMENTAL_WATCH_TAR"); ok {
		useTar, _ = strconv.ParseBool(useTarEnv)
	} else {
		useTar = true
	}
	if useTar {
		return sync.NewTar(project.Name, tarDockerClient{s: s})
	}

	return sync.NewDockerCopy(project.Name, s, s.stdinfo())
}

func (s *composeService) Watch(ctx context.Context, project *types.Project, services []string, _ api.WatchOptions) error { //nolint: gocyclo
	if err := project.ForServices(services); err != nil {
		return err
	}
	syncer := s.getSyncImplementation(project)
	eg, ctx := errgroup.WithContext(ctx)
	watching := false
	for i := range project.Services {
		service := project.Services[i]
		config, err := loadDevelopmentConfig(service, project)
		if err != nil {
			return err
		}

		if config == nil {
			continue
		}

		if len(config.Watch) > 0 && service.Build == nil {
			// service configured with watchers but no build section
			return fmt.Errorf("can't watch service %q without a build context", service.Name)
		}

		if len(services) > 0 && service.Build == nil {
			// service explicitly selected for watch has no build section
			return fmt.Errorf("can't watch service %q without a build context", service.Name)
		}

		if len(services) == 0 && service.Build == nil {
			continue
		}

		// set the service to always be built - watch triggers `Up()` when it receives a rebuild event
		service.PullPolicy = types.PullPolicyBuild
		project.Services[i] = service

		dockerIgnores, err := watch.LoadDockerIgnore(service.Build.Context)
		if err != nil {
			return err
		}

		// add a hardcoded set of ignores on top of what came from .dockerignore
		// some of this should likely be configurable (e.g. there could be cases
		// where you want `.git` to be synced) but this is suitable for now
		dotGitIgnore, err := watch.NewDockerPatternMatcher("/", []string{".git/"})
		if err != nil {
			return err
		}
		ignore := watch.NewCompositeMatcher(
			dockerIgnores,
			watch.EphemeralPathMatcher(),
			dotGitIgnore,
		)

		var paths []string
		for _, trigger := range config.Watch {
			if checkIfPathAlreadyBindMounted(trigger.Path, service.Volumes) {
				logrus.Warnf("path '%s' also declared by a bind mount volume, this path won't be monitored!\n", trigger.Path)
				continue
			}
			paths = append(paths, trigger.Path)
		}

		watcher, err := watch.NewWatcher(paths, ignore)
		if err != nil {
			return err
		}

		fmt.Fprintf(s.stdinfo(), "watching %s\n", paths)
		err = watcher.Start()
		if err != nil {
			return err
		}
		watching = true

		eg.Go(func() error {
			defer watcher.Close() //nolint:errcheck
			return s.watch(ctx, project, service.Name, watcher, syncer, config.Watch)
		})
	}

	if !watching {
		return fmt.Errorf("none of the selected services is configured for watch, consider setting an 'x-develop' section")
	}

	return eg.Wait()
}

func (s *composeService) watch(
	ctx context.Context,
	project *types.Project,
	name string,
	watcher watch.Notify,
	syncer sync.Syncer,
	triggers []Trigger,
) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	ignores := make([]watch.PathMatcher, len(triggers))
	for i, trigger := range triggers {
		ignore, err := watch.NewDockerPatternMatcher(trigger.Path, trigger.Ignore)
		if err != nil {
			return err
		}
		ignores[i] = ignore
	}

	events := make(chan fileEvent)
	batchEvents := batchDebounceEvents(ctx, s.clock, quietPeriod, events)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case batch := <-batchEvents:
				start := time.Now()
				logrus.Debugf("batch start: service[%s] count[%d]", name, len(batch))
				if err := s.handleWatchBatch(ctx, project, name, batch, syncer); err != nil {
					logrus.Warnf("Error handling changed files for service %s: %v", name, err)
				}
				logrus.Debugf("batch complete: service[%s] duration[%s] count[%d]",
					name, time.Since(start), len(batch))
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-watcher.Errors():
			return err
		case event := <-watcher.Events():
			hostPath := event.Path()
			for i, trigger := range triggers {
				logrus.Debugf("change for %s - comparing with %s", hostPath, trigger.Path)
				if fileEvent := maybeFileEvent(trigger, hostPath, ignores[i]); fileEvent != nil {
					events <- *fileEvent
				}
			}
		}
	}
}

// maybeFileEvent returns a file event object if hostPath is valid for the provided trigger and ignore
// rules.
//
// Any errors are logged as warnings and nil (no file event) is returned.
func maybeFileEvent(trigger Trigger, hostPath string, ignore watch.PathMatcher) *fileEvent {
	if !watch.IsChild(trigger.Path, hostPath) {
		return nil
	}
	isIgnored, err := ignore.Matches(hostPath)
	if err != nil {
		logrus.Warnf("error ignore matching %q: %v", hostPath, err)
		return nil
	}

	if isIgnored {
		logrus.Debugf("%s is matching ignore pattern", hostPath)
		return nil
	}

	var containerPath string
	if trigger.Target != "" {
		rel, err := filepath.Rel(trigger.Path, hostPath)
		if err != nil {
			logrus.Warnf("error making %s relative to %s: %v", hostPath, trigger.Path, err)
			return nil
		}
		// always use Unix-style paths for inside the container
		containerPath = path.Join(trigger.Target, rel)
	}

	return &fileEvent{
		Action: WatchAction(trigger.Action),
		PathMapping: sync.PathMapping{
			HostPath:      hostPath,
			ContainerPath: containerPath,
		},
	}
}

func loadDevelopmentConfig(service types.ServiceConfig, project *types.Project) (*DevelopmentConfig, error) {
	var config DevelopmentConfig
	y, ok := service.Extensions["x-develop"]
	if !ok {
		return nil, nil
	}
	err := mapstructure.Decode(y, &config)
	if err != nil {
		return nil, err
	}
	baseDir, err := filepath.EvalSymlinks(project.WorkingDir)
	if err != nil {
		return nil, fmt.Errorf("resolving symlink for %q: %w", project.WorkingDir, err)
	}

	for i, trigger := range config.Watch {
		if !filepath.IsAbs(trigger.Path) {
			trigger.Path = filepath.Join(baseDir, trigger.Path)
		}
		if p, err := filepath.EvalSymlinks(trigger.Path); err == nil {
			// this might fail because the path doesn't exist, etc.
			trigger.Path = p
		}
		trigger.Path = filepath.Clean(trigger.Path)
		if trigger.Path == "" {
			return nil, errors.New("watch rules MUST define a path")
		}

		if trigger.Action == string(WatchActionRebuild) && service.Build == nil {
			return nil, fmt.Errorf("service %s doesn't have a build section, can't apply 'rebuild' on watch", service.Name)
		}

		config.Watch[i] = trigger
	}
	return &config, nil
}

// batchDebounceEvents groups identical file events within a sliding time window and writes the results to the returned
// channel.
//
// The returned channel is closed when the debouncer is stopped via context cancellation or by closing the input channel.
func batchDebounceEvents(ctx context.Context, clock clockwork.Clock, delay time.Duration, input <-chan fileEvent) <-chan []fileEvent {
	out := make(chan []fileEvent)
	go func() {
		defer close(out)
		seen := make(map[fileEvent]time.Time)
		flushEvents := func() {
			if len(seen) == 0 {
				return
			}
			events := make([]fileEvent, 0, len(seen))
			for e := range seen {
				events = append(events, e)
			}
			// sort batch by oldest -> newest
			// (if an event is seen > 1 per batch, it gets the latest timestamp)
			sort.SliceStable(events, func(i, j int) bool {
				x := events[i]
				y := events[j]
				return seen[x].Before(seen[y])
			})
			out <- events
			seen = make(map[fileEvent]time.Time)
		}

		t := clock.NewTicker(delay)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.Chan():
				flushEvents()
			case e, ok := <-input:
				if !ok {
					// input channel was closed
					flushEvents()
					return
				}
				seen[e] = time.Now()
				t.Reset(delay)
			}
		}
	}()
	return out
}

func checkIfPathAlreadyBindMounted(watchPath string, volumes []types.ServiceVolumeConfig) bool {
	for _, volume := range volumes {
		if volume.Bind != nil && strings.HasPrefix(watchPath, volume.Source) {
			return true
		}
	}
	return false
}

type tarDockerClient struct {
	s *composeService
}

func (t tarDockerClient) ContainersForService(ctx context.Context, projectName string, serviceName string) ([]moby.Container, error) {
	containers, err := t.s.getContainers(ctx, projectName, oneOffExclude, true, serviceName)
	if err != nil {
		return nil, err
	}
	return containers, nil
}

func (t tarDockerClient) Exec(ctx context.Context, containerID string, cmd []string, in io.Reader) error {
	execCfg := moby.ExecConfig{
		Cmd:          cmd,
		AttachStdout: false,
		AttachStderr: true,
		AttachStdin:  in != nil,
		Tty:          false,
	}
	execCreateResp, err := t.s.apiClient().ContainerExecCreate(ctx, containerID, execCfg)
	if err != nil {
		return err
	}

	startCheck := moby.ExecStartCheck{Tty: false, Detach: false}
	conn, err := t.s.apiClient().ContainerExecAttach(ctx, execCreateResp.ID, startCheck)
	if err != nil {
		return err
	}
	defer conn.Close()

	var eg errgroup.Group
	if in != nil {
		eg.Go(func() error {
			defer func() {
				_ = conn.CloseWrite()
			}()
			_, err := io.Copy(conn.Conn, in)
			return err
		})
	}
	eg.Go(func() error {
		_, err := io.Copy(t.s.stdinfo(), conn.Reader)
		return err
	})

	err = t.s.apiClient().ContainerExecStart(ctx, execCreateResp.ID, startCheck)
	if err != nil {
		return err
	}

	// although the errgroup is not tied directly to the context, the operations
	// in it are reading/writing to the connection, which is tied to the context,
	// so they won't block indefinitely
	if err := eg.Wait(); err != nil {
		return err
	}

	execResult, err := t.s.apiClient().ContainerExecInspect(ctx, execCreateResp.ID)
	if err != nil {
		return err
	}
	if execResult.Running {
		return errors.New("process still running")
	}
	if execResult.ExitCode != 0 {
		return fmt.Errorf("exit code %d", execResult.ExitCode)
	}
	return nil
}

func (s *composeService) handleWatchBatch(
	ctx context.Context,
	project *types.Project,
	serviceName string,
	batch []fileEvent,
	syncer sync.Syncer,
) error {
	pathMappings := make([]sync.PathMapping, len(batch))
	for i := range batch {
		if batch[i].Action == WatchActionRebuild {
			fmt.Fprintf(
				s.stdinfo(),
				"Rebuilding %s after changes were detected:%s\n",
				serviceName,
				strings.Join(append([]string{""}, batch[i].HostPath), "\n  - "),
			)
			err := s.Up(ctx, project, api.UpOptions{
				Create: api.CreateOptions{
					Build: &api.BuildOptions{
						Pull: false,
						Push: false,
						// restrict the build to ONLY this service, not any of its dependencies
						Services: []string{serviceName},
					},
					Services: []string{serviceName},
					Inherit:  true,
				},
				Start: api.StartOptions{
					Services: []string{serviceName},
					Project:  project,
				},
			})
			if err != nil {
				fmt.Fprintf(s.stderr(), "Application failed to start after update\n")
			}
			return nil
		}
		pathMappings[i] = batch[i].PathMapping
	}

	writeWatchSyncMessage(s.stdinfo(), serviceName, pathMappings)

	service, err := project.GetService(serviceName)
	if err != nil {
		return err
	}
	if err := syncer.Sync(ctx, service, pathMappings); err != nil {
		return err
	}
	return nil
}

// writeWatchSyncMessage prints out a message about the sync for the changed paths.
func writeWatchSyncMessage(w io.Writer, serviceName string, pathMappings []sync.PathMapping) {
	const maxPathsToShow = 10
	if len(pathMappings) <= maxPathsToShow || logrus.IsLevelEnabled(logrus.DebugLevel) {
		hostPathsToSync := make([]string, len(pathMappings))
		for i := range pathMappings {
			hostPathsToSync[i] = pathMappings[i].HostPath
		}
		fmt.Fprintf(
			w,
			"Syncing %s after changes were detected:%s\n",
			serviceName,
			strings.Join(append([]string{""}, hostPathsToSync...), "\n  - "),
		)
	} else {
		hostPathsToSync := make([]string, len(pathMappings))
		for i := range pathMappings {
			hostPathsToSync[i] = pathMappings[i].HostPath
		}
		fmt.Fprintf(
			w,
			"Syncing %s after %d changes were detected\n",
			serviceName,
			len(pathMappings),
		)
	}
}
