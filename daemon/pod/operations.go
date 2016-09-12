package pod

import (
	"fmt"
	"path"
	"syscall"
	"time"

	"github.com/golang/glog"
	"github.com/docker/docker/pkg/signal"

	apitypes "github.com/hyperhq/hyperd/types"
	"github.com/hyperhq/hyperd/storage"
	"github.com/hyperhq/hyperd/storage/devicemapper"
	runv "github.com/hyperhq/runv/api"
	"github.com/hyperhq/runv/hypervisor"
	"github.com/docker/engine-api/types"
)

type sandboxOp func(sb *hypervisor.Vm) error
type stateValidator func(state int) bool

func (p *Pod) protectedSandboxOperation(op sandboxOp, validator stateValidator, timeout time.Duration, comment string) error {
	errChan := make(chan error, 1)
	p.status.sLock.RLock()
	if !validator(p.status.pod) {
		err := fmt.Errorf("%s: pod state is not valid: %d", comment, p.status.pod)
		glog.Error(err)
		errChan <- err
	} else if p.sandbox != nil {
		go func(sb *hypervisor.Vm) {
			defer func() {
				err := recover()
				if err != nil {
					glog.Error(err)
					errChan <- err
				}
			}()
			errChan <- op()
		}(p.sandbox)
	} else {
		glog.Warningf("%s (%s): sandbox not existed", comment, p.Name)
		errChan <- nil
	}
	p.status.sLock.RUnlock()

	var timeoutChan <-chan time.Time
	if timeout < 0 {
		timeoutChan = make(chan time.Time, 1)
	} else {
		timeoutChan = time.After(timeout)
	}

	select {
	case err, ok := <- errChan:
		if !ok {
			err := fmt.Errorf("%s: failed to get operation result", comment)
			glog.Error(err)
			return err
		}
		return err
	case <- timeoutChan:
		err := fmt.Errorf("%s: timeout (%v) during waiting operation result", comment, timeout)
		glog.Error(err)
		return err
	}
}

func (p *Pod) Kill(id string, sig int64) error {
	return p.protectedSandboxOperation(
		func(sb *hypervisor.Vm) error {
			return sb.KillContainer(id, syscall.Signal(sig))
		},
		func(state int) bool {
			return true
		},
		time.Second * 5,
		fmt.Sprintf("Kill container %s with %d", id, sig))
}

func (p *Pod) ForceQuit() {
	p.protectedSandboxOperation(
		func(sb *hypervisor.Vm) error {
			sb.Kill()
			return nil
		},
		func(state int) bool {
			return true
		},
		time.Second * 5,
		fmt.Sprintf("Kill pod %s", p.Name))
}

func (p *Pod) Pause() error {
	err := p.protectedSandboxOperation(
		func(sb *hypervisor.Vm) error {
			return sb.Pause(true)
		},
		func(state int) bool {
			return state == S_POD_RUNNING
		},
		time.Second * 5,
		fmt.Sprintf("Pause pod %s", p.Name))

	if err == nil && !p.status.Pause() {
		err = fmt.Errorf("%s status changed during pausing", p.Name)
		glog.Error(err)
	}

	return err
}

func (p *Pod) UnPause() error {
	err := p.protectedSandboxOperation(
		func(sb *hypervisor.Vm) error {
			return sb.Pause(false)
		},
		func(state int) bool {
			return state == S_POD_PAUSED
		},
		time.Second * 5,
		fmt.Sprintf("Pause pod %s", p.Name))

	if err == nil && !p.status.Unpause() {
		err = fmt.Errorf("%s status changed during unpausing", p.Name)
		glog.Error(err)
	}

	return err
}

func (p *Pod) Stop(graceful int) error {
	if begin := p.status.Stop(); !begin {
		err := fmt.Errorf("pod %s is not running for stop container", p.Name)
		glog.Error(err)
		return err
	}

	p.stopAllContainers(graceful)

	err := p.protectedSandboxOperation(
		func(sb *hypervisor.Vm) error {
			rsp := sb.Shutdown()
			if rsp.IsSuccess() {
				glog.Infof("pod %s is stopped", p.Name)
				return nil
			}
			err := fmt.Errorf("failed to shuting down %s: %s", p.Name, rsp.Message())
			glog.Error(err)
			return err
		},
		func(state int) bool {
			return true
		},
		time.Second * time.Duration(graceful),
		fmt.Sprintf("Stop pod %s", p.Name))

	if err == nil {
		p.status.Stopped()
	}

	p.status.ToError(S_POD_STOPPING)
	return err
}

func (p *Pod) stopAllContainers(graceful int) {

	type containerException struct{
		id string
		err error
	}

	var (
		resChan <-chan *runv.ProcessExit
		errChan chan *containerException
	)

	p.status.sLock.RLock() // be carefore , begin lock, don't return before unlock

	wm := map[string]syscall.Signal{}
	tbs := []string{}
	for _, cs := range p.status.containers {
		if cs.IsRunning() {
			cdesc, _ := p.runtimeConfig.containers[cs.Id]
			wm[cs.Id] = StringToSignal(cdesc.StopSignal)
			tbs = append(tbs, cs.Id)
		}
	}

	if len(wm) > 0 {
		resChan = p.sandbox.WaitProcess(true, tbs, graceful)
		errChan = make(chan *containerException, len(wm))
		if resChan != nil {
			for id, sig := range wm {
				go func (sb *hypervisor.Vm, id string, sig syscall.Signal) {
					defer func() {
						if pe := recover(); pe != nil {
							err := fmt.Errorf("panic during killing container %s: %v", id, pe)
							glog.Error(err)
							errChan <- &containerException{id, err}
						}
					}()
					glog.V(1).Infof("stopping %s: killing container %s with %d", p.Name, id, sig)
					err := p.sandbox.KillContainer(id, sig)
					if err != nil {
						glog.Error(err)
						errChan <- &containerException{id, err}
					}
				} (p.sandbox, id, sig)
			}
		} else {
			glog.Warningf("stopping %s: cannot wait containers %v", p.Name, tbs)
		}
	}

	p.status.sLock.RUnlock() // be carefore , end lock, don't return before unlock

	for len(wm) > 0 && resChan != nil {
		select{
		case ex, ok := <-resChan:
			if !ok {
				glog.Warningf("%s containers stop timeout or container waiting channel broken", p.Name)
				resChan = nil
				break
			}
			glog.V(1).Infof("container %s stopped", ex.Id)
			delete(wm, ex.Id)
		case e := <- errChan:
			delete(wm, e.id)
		}

	}
}

func (p *Pod) StopContainer(id string, graceful int) error {
	if !p.IsAlive() {
		err := fmt.Errorf("pod %s is not running for stop container", p.Name)
		glog.Error(err)
		return err
	}

	if graceful <0 {
		graceful = 5
	}

	w := p.sandbox.WaitProcess(true, []string{id}, graceful)
	if w == nil {
		err := fmt.Errorf("cannot wait container %s of pod %s", id, p.Name)
		glog.Error(err)
		return err
	}

	errChan := make(chan error, 1)
	func() {
		p.status.sLock.RLock()
		defer p.status.sLock.RUnlock()

		cdesc, ok := p.runtimeConfig.containers[id]
		if !ok {
			err := fmt.Errorf("stop: can not find runtime config of container %s of pod %s", id, p.Name)
			errChan <- err
			return
		}

		cs, ok := p.status.containers[id]
		if !ok {
			err := fmt.Errorf("stop: can not find status of container %s of pod %s", id, p.Name)
			errChan <- err
			return
		}

		if !cs.Stop() {
			err := fmt.Errorf("stop: cannot stop container %s, current state: %d", id, cs.CurrentState())
			glog.Error(err)
			errChan <- err
			return
		}

		go func(sb *hypervisor.Vm) {
			defer func() {
				err := recover()
				if err != nil {
					glog.Error(err)
					errChan <- err
				}
			}()
			glog.V(1).Infof("pod %s: kill container %s with signal %s", p.Name, id, cdesc.StopSignal)
			err := sb.KillContainer(id, StringToSignal(cdesc.StopSignal))
			errChan <- err
		} (p.sandbox)
	}()

	err := <- errChan
	if err != nil {
		glog.Error(err)
		return err
	}

	r, ok := <-w
	if !ok {
		glog.Warningf("wait container %s failed, kill it", id)
		return p.sandbox.KillContainer(id, syscall.SIGKILL)
	}

	glog.Infof("container %s was stopped with exit code %d [%v]", id, r.Code, r.FinishedAt)
	return nil
}

func (p *Pod) Remove() {

}

func (p *Pod) removeRuntimeContainer(desc *runv.ContainerDescription) {
	r := p.sandbox.RemoveContainer(desc.Id)
	if r.IsSuccess() {
		glog.Errorf("failed to remove container %s: %s", desc.Id, r.Message())
	}
}

func (p *Pod) umountContainer(desc *runv.ContainerDescription) {
	var (
		sharedDir = path.Join(hypervisor.BaseDir, p.Name, hypervisor.ShareDirTag)
	)
	p.factory.sd.CleanupContainer(desc.Id, sharedDir)
}

func (p *Pod) removeContainerVolumes(desc *runv.ContainerDescription) {
	var (
		sharedDir = path.Join(hypervisor.BaseDir, p.Name, hypervisor.ShareDirTag)
	)

	p.status.sLock.Lock()
	defer p.status.sLock.Unlock()

	vols := []string{}
	for v := range desc.Volumes {
		vols = append(vols, v)
	}
	if len(vols) == 0 {
		return
	}

	if p.sandbox != nil {
		_, rvols := p.sandbox.RemoveVolumes(vols...)
		glog.V(1).Infof("remove volumes of container %s: %#v", desc.Id, rvols)
		for v, res := range rvols {
			if res.IsSuccess() || res.Message() == "in use" {
				glog.Warningf("volume %s of container %s is in use, would not be deleted", v, desc.Id)
				delete(desc.Volumes, v)
			}
		}
	}

	for v := range desc.Volumes {
		if vd, ok := p.runtimeConfig.volumes[v]; ok {
			//should encap in storage drivers
			if vd.IsDir() {
				storage.UmountVFSVolume(vd.Source, sharedDir)
			} else if p.factory.sd.Type() == "devicemapper" {
				devicemapper.UnmapVolume(vd.Source)
			}
		}
		delete(p.status.volumes, v)
		delete(p.runtimeConfig.volumes, v)
	}
}

func (p *Pod) RemoveContainer(id string, delete bool) error {
	var (
		spec *apitypes.UserContainer
		desc *runv.ContainerDescription
		status *ContainerStatus
		session *ContainerSession
	)

	func() { // use a func to set the scope of defer
		p.status.sLock.Lock()
		defer p.status.sLock.Unlock()

		remains := []*apitypes.UserContainer{}
		for i, c := range p.spec.Containers {
			if c.Id == id {
				spec = c
				remains = append(remains, p.spec.Containers[(i+1):]...)
				break
			}
			remains = append(remains, c)
		}
		p.spec.Containers = remains

		desc, _ = p.runtimeConfig.containers[id]
		delete(p.runtimeConfig.containers, id)

		status, _ = p.status.containers[id]
		delete(p.status.containers, id)

		session, _ = p.sessions[id]
		delete(p.sessions, id)
	} ()

	if p.IsAlive() && status != nil && desc != nil && status.IsAlive() {
		// remove container from sandbox p.sandbox.
		p.removeRuntimeContainer(desc)
	}
	p.umountContainer(desc)
	if session != nil && session.log.Driver != nil {
		session.log.Driver.Close()
	}

	if delete {
		p.factory.engine.ContainerRm(id, & types.ContainerRmConfig{true, false, false})
	}

	p.removeContainerVolumes(desc)

	return nil
}

func (p *Pod) RenameContainer(id, name string) {
	for _, c := range p.spec.Containers {
		if c.Id == id {
			c.Name = name
			break
		}
	}
	if c, ok := p.runtimeConfig.containers[id]; ok {
		c.Name = "/" + name
	}
}

func StringToSignal(s string) syscall.Signal {
	sig, ok := signal.SignalMap[s]
	if !ok {
		sig = syscall.SIGTERM
	}
	return sig
}