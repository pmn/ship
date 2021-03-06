package kustomize

import (
	"context"
	"os"
	"strings"

	"time"

	"path"

	"path/filepath"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	ktypes "github.com/kubernetes-sigs/kustomize/pkg/types"
	"github.com/pkg/errors"
	"github.com/replicatedhq/ship/pkg/api"
	"github.com/replicatedhq/ship/pkg/lifecycle"
	"github.com/replicatedhq/ship/pkg/lifecycle/daemon/daemontypes"
	"github.com/replicatedhq/ship/pkg/state"
	"github.com/spf13/afero"
	"gopkg.in/yaml.v2"
)

func NewDaemonKustomizer(
	logger log.Logger,
	daemon daemontypes.Daemon,
	fs afero.Afero,
	stateManager state.Manager,
) lifecycle.Kustomizer {
	return &daemonkustomizer{
		Kustomizer: Kustomizer{
			Logger: logger,
			FS:     fs,
			State:  stateManager,
		},
		Daemon: daemon,
	}
}

// kustomizer will *try* to pull in the Kustomizer libs from kubernetes-sigs/kustomize,
// if not we'll have to fork. for now it just explodes
type daemonkustomizer struct {
	Kustomizer
	Daemon daemontypes.Daemon
}

func (l *daemonkustomizer) Execute(ctx context.Context, release *api.Release, step api.Kustomize) error {
	debug := level.Debug(log.With(l.Logger, "struct", "kustomizer", "method", "execute"))

	daemonExitedChan := l.Daemon.EnsureStarted(ctx, release)

	debug.Log("event", "daemon.started")

	l.Daemon.PushKustomizeStep(ctx, daemontypes.Kustomize{
		BasePath: step.BasePath,
	})
	debug.Log("event", "step.pushed")

	if err := l.writeBase(step); err != nil {
		return errors.Wrap(err, "write base kustomization")
	}

	err := l.awaitKustomizeSaved(ctx, daemonExitedChan)
	debug.Log("event", "kustomize.saved", "err", err)
	if err != nil {
		return errors.Wrap(err, "await save kustomize")
	}

	current, err := l.State.TryLoad()
	if err != nil {
		return errors.Wrap(err, "load state")
	}

	debug.Log("event", "state.loaded")
	kustomizeState := current.CurrentKustomize()

	var shipOverlay state.Overlay
	if kustomizeState == nil {
		debug.Log("event", "state.kustomize.empty")
	} else {
		shipOverlay = kustomizeState.Ship()
	}

	debug.Log("event", "mkdir", "dir", step.Dest)
	err = l.FS.MkdirAll(step.Dest, 0777)
	if err != nil {
		debug.Log("event", "mkdir.fail", "dir", step.Dest)
		return errors.Wrapf(err, "make dir %s", step.Dest)
	}

	relativePatchPaths, err := l.writePatches(shipOverlay, step.Dest)
	if err != nil {
		return err
	}

	err = l.writeOverlay(step, relativePatchPaths)
	if err != nil {
		return errors.Wrap(err, "write overlay")
	}

	return nil
}

func (l *daemonkustomizer) awaitKustomizeSaved(ctx context.Context, daemonExitedChan chan error) error {
	debug := level.Debug(log.With(l.Logger, "struct", "kustomizer", "method", "kustomize.save.await"))
	for {
		select {
		case <-ctx.Done():
			debug.Log("event", "ctx.done")
			return ctx.Err()
		case err := <-daemonExitedChan:
			debug.Log("event", "daemon.exit")
			if err != nil {
				return err
			}
			return errors.New("daemon exited")
		case <-l.Daemon.KustomizeSavedChan():
			debug.Log("event", "kustomize.finalized")
			return nil
		case <-time.After(10 * time.Second):
			debug.Log("waitingFor", "kustomize.finalized")
		}
	}
}

func (l *Kustomizer) writePatches(shipOverlay state.Overlay, destDir string) (relativePatchPaths []string, err error) {
	debug := level.Debug(log.With(l.Logger, "method", "writePatches"))

	for file, contents := range shipOverlay.Patches {
		name := path.Join(destDir, file)
		err := l.writePatch(name, contents)
		if err != nil {
			debug.Log("event", "write", "name", name)
			return []string{}, errors.Wrapf(err, "write %s", name)
		}

		relativePatchPath, err := filepath.Rel(destDir, name)
		if err != nil {
			return []string{}, errors.Wrap(err, "unable to determine relative path")
		}
		relativePatchPaths = append(relativePatchPaths, relativePatchPath)
	}
	return relativePatchPaths, nil
}

func (l *Kustomizer) writePatch(name string, contents string) error {
	debug := level.Debug(log.With(l.Logger, "method", "writePatch"))

	destDir := filepath.Dir(name)

	// make the dir
	err := l.FS.MkdirAll(destDir, 0777)
	if err != nil {
		debug.Log("event", "mkdir.fail", "dir", destDir)
		return errors.Wrapf(err, "make dir %s", destDir)
	}

	// write the file
	err = l.FS.WriteFile(name, []byte(contents), 0666)
	if err != nil {
		return errors.Wrapf(err, "write patch %s", name)
	}
	debug.Log("event", "patch.written", "patch", name)
	return nil
}

func (l *Kustomizer) writeOverlay(step api.Kustomize, relativePatchPaths []string) error {
	// just always make a new kustomization.yaml for now
	kustomization := ktypes.Kustomization{
		Bases: []string{
			filepath.Join("../../", step.BasePath),
		},
		Patches: relativePatchPaths,
	}

	marshalled, err := yaml.Marshal(kustomization)
	if err != nil {
		return errors.Wrap(err, "marshal kustomization.yaml")
	}

	name := path.Join(step.Dest, "kustomization.yaml")
	err = l.FS.WriteFile(name, []byte(marshalled), 0666)
	if err != nil {
		return errors.Wrapf(err, "write file %s", name)
	}

	return nil
}

func (l *Kustomizer) writeBase(step api.Kustomize) error {
	debug := level.Debug(log.With(l.Logger, "method", "writeBase"))

	baseKustomization := ktypes.Kustomization{}
	if err := l.FS.Walk(
		step.BasePath,
		func(targetPath string, info os.FileInfo, err error) error {
			if err != nil {
				debug.Log("event", "walk.fail", "path", targetPath)
				return errors.Wrap(err, "failed to walk path")
			}
			if filepath.Ext(targetPath) == ".yaml" && !strings.HasSuffix(targetPath, "kustomization.yaml") {
				relativePath, err := filepath.Rel(step.BasePath, targetPath)
				if err != nil {
					debug.Log("event", "relativepath.fail", "base", step.BasePath, "target", targetPath)
					return errors.Wrap(err, "failed to get relative path")
				}
				baseKustomization.Resources = append(baseKustomization.Resources, relativePath)
			}
			return nil
		},
	); err != nil {
		return err
	}

	if len(baseKustomization.Resources) == 0 {
		return errors.New("Base directory is empty")
	}

	marshalled, err := yaml.Marshal(baseKustomization)
	if err != nil {
		return errors.Wrap(err, "marshal base kustomization.yaml")
	}

	// write base kustomization
	name := path.Join(step.BasePath, "kustomization.yaml")
	err = l.FS.WriteFile(name, []byte(marshalled), 0666)
	if err != nil {
		return errors.Wrapf(err, "write file %s", name)
	}
	return nil
}
