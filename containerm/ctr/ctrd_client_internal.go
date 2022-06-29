// Copyright The PouchContainer Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package name changed also removed not needed IOs logic and added custom code to handle the specific use case, Bosch.IO GmbH, 2020

package ctr

import (
	"context"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/runtime"
	"github.com/containerd/imgcrypt"
	"github.com/containerd/imgcrypt/images/encryption"
	"github.com/containerd/typeurl"
	"github.com/eclipse-kanto/container-management/containerm/containers/types"
	"github.com/eclipse-kanto/container-management/containerm/log"
	"github.com/eclipse-kanto/container-management/containerm/util"
)

func (ctrdClient *containerdClient) generateRemoteOpts(imageInfo types.Image) []containerd.RemoteOpt {
	remoteOpts := []containerd.RemoteOpt{
		containerd.WithSchema1Conversion,
	}
	resolver := ctrdClient.registriesResolver.ResolveImageRegistry(util.GetImageHost(imageInfo.Name))
	if resolver != nil {
		remoteOpts = append(remoteOpts, containerd.WithResolver(resolver))
	} else {
		log.Warn("the default resolver by containerd will be used for image %s", imageInfo.Name)
	}
	return remoteOpts
}

func (ctrdClient *containerdClient) generateUnpackOpts(imageInfo types.Image) ([]containerd.UnpackOpt, error) {
	decryptCfg, dcErr := ctrdClient.decMgr.GetDecryptConfig(imageInfo.DecryptConfig)
	if dcErr != nil {
		return nil, dcErr
	}
	var unpackOpts []containerd.UnpackOpt
	unpackOpts = append(unpackOpts,
		encryption.WithUnpackConfigApplyOpts(encryption.WithDecryptedUnpack(&imgcrypt.Payload{DecryptConfig: *decryptCfg})),
	)
	log.Debug("created decrypt unpack options for image %s", imageInfo.Name)

	return unpackOpts, nil
}

func (ctrdClient *containerdClient) generateNewContainerOpts(container *types.Container, containerImage containerd.Image) ([]containerd.NewContainerOpts, error) {
	createOpts := []containerd.NewContainerOpts{}
	createOpts = append(createOpts, WithSnapshotOpts(ctrdClient.spi.GetSnapshotID(container.ID), containerd.DefaultSnapshotter)...) // NB! It's very important to apply the snapshot configs prior to the OCI Spec ones as they are dependent
	createOpts = append(createOpts,
		WithRuntimeOpts(container, ctrdClient.rootExec),
		WithSpecOpts(container, containerImage, ctrdClient.rootExec))

	decryptCfg, err := ctrdClient.decMgr.GetDecryptConfig(container.Image.DecryptConfig)
	if err != nil {
		return nil, err
	}
	createOpts = append(createOpts, encryption.WithAuthorizationCheck(decryptCfg))

	return createOpts, nil
}

func (ctrdClient *containerdClient) getImage(ctx context.Context, imageInfo types.Image) (containerd.Image, error) {
	decryptConfig, err := ctrdClient.decMgr.GetDecryptConfig(imageInfo.DecryptConfig)
	if err != nil {
		return nil, err
	}
	ctrdImage, err := ctrdClient.spi.GetImage(ctx, imageInfo.Name)
	if err != nil {
		return nil, err
	}

	if err = ctrdClient.decMgr.CheckAuthorization(ctx, ctrdImage, decryptConfig); err != nil {
		return nil, err
	}
	return ctrdImage, nil
}

func (ctrdClient *containerdClient) pullImage(ctx context.Context, imageInfo types.Image) (containerd.Image, error) {
	dc, dcErr := ctrdClient.decMgr.GetDecryptConfig(imageInfo.DecryptConfig)
	if dcErr != nil {
		return nil, dcErr
	}
	ctrdImage, err := ctrdClient.spi.GetImage(ctx, imageInfo.Name)
	if err != nil {
		// if the image is not present locally - pull it
		if errdefs.IsNotFound(err) {
			ctrdImage, err = ctrdClient.spi.PullImage(ctx, imageInfo.Name, ctrdClient.generateRemoteOpts(imageInfo)...)
			if err != nil {
				return nil, err
			}
			// NB! It's really important to have the logic of pulling and unpacking separate
			// Reasoning - before unpacking the content (which is a consuming operation to revert)
			// it's essential to perform an authorization check to prevent leased content leaks
			if checkErr := ctrdClient.decMgr.CheckAuthorization(ctx, ctrdImage, dc); checkErr != nil {
				return nil, checkErr
			}
			unpackOpts, optsErr := ctrdClient.generateUnpackOpts(imageInfo)
			if optsErr != nil {
				return nil, optsErr
			}
			if unpackErr := ctrdClient.spi.UnpackImage(ctx, ctrdImage, unpackOpts...); unpackErr != nil {
				return nil, unpackErr
			}
		}
	} else {
		if checkErr := ctrdClient.decMgr.CheckAuthorization(ctx, ctrdImage, dc); checkErr != nil {
			return nil, checkErr
		}
	}
	return ctrdImage, err
}

func (ctrdClient *containerdClient) createSnapshot(ctx context.Context, containerID string, image containerd.Image, imageInfo types.Image) error {
	unpackOpts, err := ctrdClient.generateUnpackOpts(imageInfo)
	if err != nil {
		log.ErrorErr(err, "error while generating unpack opts for image ID = %s used by container ID = %s", image.Name, containerID)
		return err
	}
	err = ctrdClient.spi.PrepareSnapshot(ctx, containerID, image, unpackOpts...)
	if err != nil {
		log.ErrorErr(err, "error while trying to create a snapshot for container ID = %s with image ID = %s ", containerID, image.Name)
		return err
	}
	err = ctrdClient.spi.MountSnapshot(ctx, containerID, rootFSPathDefault)
	if err != nil {
		log.ErrorErr(err, "error while trying to mount rootfs for container ID = %s , image with ID = %s ", containerID, image.Name)
		return err
	}
	return err
}

func (ctrdClient *containerdClient) clearSnapshot(ctx context.Context, containerID string) {
	if cleanupErr := ctrdClient.spi.RemoveSnapshot(ctx, containerID); cleanupErr != nil {
		log.ErrorErr(cleanupErr, "error while removing snapshot for container id = %s", containerID)
	}
	if cleanupErr := ctrdClient.spi.UnmountSnapshot(ctx, containerID, rootFSPathDefault /*until we have provided a configuration to be externally specified*/); cleanupErr != nil {
		log.ErrorErr(cleanupErr, "error while unmounting rootfs for container id = %s", containerID)
	}
}

func (ctrdClient *containerdClient) createTask(ctx context.Context, ctrIOCfg *types.IOConfig, containerID, checkpointDir string, ctrdContainer containerd.Container) (*containerInfo, error) {
	/*if checkpointDir != "" {
		//TODO: add checkpoint support for tasks
		checkpoint, err = createCheckpointDescriptor(ctx, checkpointDir, ctrdClient)
		if err != nil {
			return nil, err
		}

		defer func() {
			if checkpoint != nil {
				// remove the checkpoint blob after task start
				err := ctrdClient.ContentStore().Delete(context.Background(), checkpoint.Digest)
				if err != nil {
					log.Printf("[WARN] failed to delete temporary checkpoint entry: %s", err)
				}
			}
		}()
	}
	// create task
	 cio.NewCreator(cio.WithStdio) , withCheckpointOpt(checkpoint)*/

	cioCreator := ctrdClient.ioMgr.NewCioCreator(ctrIOCfg.Tty)
	// create task
	task, taskErr := ctrdClient.spi.CreateTask(ctx, ctrdContainer, cioCreator)
	if taskErr != nil {
		return nil, taskErr
	}

	statusCh, err := task.Wait(context.TODO())
	if err != nil {
		if _, delErr := task.Delete(ctx); delErr != nil {
			log.WarnErr(delErr, "could not delete task for container id = %s", containerID)
		}
		return nil, err
	}

	log.Info("success to create task(pid=%d) in container(%s)", task.Pid(), containerID)

	ctrdCacheInfo := &containerInfo{
		container:     ctrdContainer,
		task:          task,
		statusChannel: statusCh,
		resultChannel: make(chan exitInfo),
	}

	return ctrdCacheInfo, nil
}

func (ctrdClient *containerdClient) loadTask(ctx context.Context, containerID, checkpointDir string, ctrdContainer containerd.Container) (*containerInfo, error) {
	var (
		retryTimeout = 3 * time.Second
		errChannel   = make(chan error, 1)
		task         containerd.Task
		err          error
	)

	// retry 3 times to ensure that there is no hanging on start-up
	for i := 0; i < 3; i++ {
		contextWithCancel, cancelFunc := context.WithCancel(ctx)
		defer cancelFunc()
		go func() {
			cioAttach := ctrdClient.ioMgr.NewCioAttach(containerID)
			task, err = ctrdClient.spi.LoadTask(contextWithCancel, ctrdContainer, cioAttach)
			errChannel <- err
		}()

		select {
		case <-time.After(retryTimeout):
			if i < 2 {
				log.Warn("timed out trying to connect to shim for container id = %s, will retry %d times", containerID, 2-i)
				continue
			}
			return nil, log.NewErrorf("failed to connect to shim for container id = %s", containerID)
		case err = <-errChannel:
		}

		break
	}

	if err != nil {
		log.ErrorErr(err, "failed to get task from containerd for container id = %s", containerID)

		if !errdefs.IsNotFound(err) {
			return nil, err
		}
		return nil, log.NewErrorf("task for containerd container id = %s not found - container is also deleted", containerID)
	}

	statusCh, waitErr := task.Wait(ctx)
	if waitErr != nil {
		return nil, waitErr
	}

	ctrCacheInfo := &containerInfo{
		container:     ctrdContainer,
		task:          task,
		statusChannel: statusCh,
		resultChannel: make(chan exitInfo),
	}

	log.Debug("successfully recovered task for container id = %s", containerID)
	return ctrCacheInfo, nil
}

/* keeping the code needed for interactive exec handling if/when supported
// closeStdinIO is used to close the write side of fifo in containerd-shim.
//
// NOTE: we should use client to make rpc call directly. if we retrieve it from
// watch, it might return 404 because the pack is saved into cache after Start.
func (client *containerdClient) closeStdinIO(containerID, processID string) error {

	ctx := client.ctrdWrapper.ensureNamespace(context.Background())
	wrapperCli := client.ctrdWrapper

	cli := wrapperCli.client
	cntr, err := cli.LoadContainer(ctx, containerID)
	if err != nil {
		return err
	}

	t, err := cntr.Task(ctx, nil)
	if err != nil {
		return err
	}

	p, err := t.LoadProcess(ctx, processID, nil)
	if err != nil {
		return err
	}

	return p.CloseIO(ctx, containerd.WithStdinCloser)
}*/

func (ctrdClient *containerdClient) initLogDriver(container *types.Container) error {
	logDriver, err := ctrdClient.logsMgr.GetLogDriver(container)
	if err != nil {
		return err
	}
	return ctrdClient.ioMgr.ConfigureIO(container.ID, logDriver, container.HostConfig.LogConfig.ModeConfig)
}

func (ctrdClient *containerdClient) killTask(ctx context.Context, ctrInfo *containerInfo, stopOpts *types.StopOpts) (int64, time.Time, error) {
	signal := util.ToSignal(stopOpts.Signal)
	timeout := time.Duration(stopOpts.Timeout) * time.Second

	if syscall.SIGKILL == signal {
		return ctrdClient.killTaskForced(ctx, ctrInfo, timeout)
	}
	signalName := unix.SignalName(signal)
	if signalName == "" {
		signalName = signal.String()
	}

	log.Debug("will send %s to the container's root process for container ID = %s", signalName, ctrInfo.c.ID)
	if err := ctrInfo.getTask().Kill(ctx, signal); err != nil {
		return -1, time.Now(), err
	}
	select {
	case exitInfo := <-ctrInfo.resultChannel:
		return int64(exitInfo.exitCode), exitInfo.exitTime, exitInfo.exitError
	case <-time.After(timeout):
		if !stopOpts.Force {
			log.Error("timed out waiting for container with ID = %s to handle %s [waited: %s]", ctrInfo.c.ID, signalName, timeout)
			return -1, time.Now(), log.NewErrorf("could not stop container with ID = %s with %s", ctrInfo.c.ID, signalName)
		}
		log.Warn("timed out waiting for container with ID = %s to handle %s", ctrInfo.c.ID, signalName)
		return ctrdClient.killTaskForced(ctx, ctrInfo, timeout)
	}
}

func (ctrdClient *containerdClient) killTaskForced(ctx context.Context, ctrInfo *containerInfo, timeout time.Duration) (int64, time.Time, error) {
	log.Debug("will try to stop the container with ID = %s using SIGKILL", ctrInfo.c.ID)
	if err := ctrInfo.getTask().Kill(ctx, syscall.SIGKILL, containerd.WithKillAll); err != nil {
		return -1, time.Now(), err
	}
	select {
	case killInfo := <-ctrInfo.resultChannel:
		log.Debug("stop with SIGKILL succeeded for container with ID = %s", ctrInfo.c.ID)
		return int64(killInfo.exitCode), killInfo.exitTime, killInfo.exitError
	case <-time.After(timeout):
		log.Error("timed out waiting for container with ID = %s to handle SIGKILL", ctrInfo.c.ID)
		return -1, time.Now(), log.NewErrorf("could not stop container with ID = %s with SIGKILL", ctrInfo.c.ID)
	}
}

func (ctrdClient *containerdClient) processEvents(namespace string) {
	ctx := context.Background()
	ctx, ctrdClient.eventsCancel = context.WithCancel(ctx)
	ch, errs := ctrdClient.spi.Subscribe(ctx, "namespace=="+namespace+",topic~=tasks/oom.*")
	for {
		select {
		case env := <-ch:
			if env.Topic != runtime.TaskOOMEventTopic && env.Namespace != namespace {
				log.Debug("skip envelope with topic %s:", env.Topic)
				continue
			}
			event, err := typeurl.UnmarshalAny(env.Event)
			if err != nil {
				log.Error("failed to unmarshal envelope %s: %v", env.Topic, err)
				continue
			}
			oomEvent, ok := event.(*events.TaskOOM)
			if !ok {
				log.Error("failed to parse %s envelope: %#v", runtime.TaskOOMEventTopic, event)
				continue
			}

			var ctrInfo *containerInfo
			if ctrInfo = ctrdClient.ctrdCache.get(oomEvent.ContainerID); ctrInfo == nil {
				log.Warn("missing container info for container - %s", oomEvent.ContainerID)
				continue
			}
			ctrInfo.setOOMKilled(true)

		case err := <-errs:
			if err != nil {
				log.Error("failed to receive envelope: %v", err)
			}
			return
		}
	}
}
