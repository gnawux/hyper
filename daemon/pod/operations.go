package pod

import (
	"fmt"
	"syscall"

	"github.com/golang/glog"
	"github.com/docker/docker/pkg/signal"
)

func (p *Pod) Kill(id string, sig int64) error {
	p.status.lock.RLock()
	defer p.status.lock.RUnlock()

	if p.sandbox == nil {
		glog.Warningf("kill %s (%s): sandbox not existed", id, p.Name)
		return nil
	}

	return p.sandbox.KillContainer(id, syscall.Signal(sig))
}

func (p *Pod) ForceQuit() {
	p.status.lock.RLock()
	defer p.status.lock.RUnlock()

	if p.sandbox == nil {
		glog.Warningf("force quit %s: sandbox not existed", p.Name)
		return nil
	}

	p.sandbox.Kill()
}

func (p *Pod) Pause() error {
	p.status.lock.Lock()
	defer p.status.lock.Unlock()

	if p.status.pod != S_POD_RUNNING || p.sandbox == nil {
		err := fmt.Errorf("%s is not running, cannot pause", p.Name)
		glog.Error(err)
		return err
	}

	err := p.sandbox.Pause(true)
	if err != nil {
		glog.Error(err)
		return err
	}

	p.status.pod = S_POD_PAUSING
	return nil
}

func (p *Pod) UnPause() error {
	p.status.lock.Lock()
	defer p.status.lock.Unlock()

	if p.status.pod != S_POD_PAUSING || p.sandbox == nil {
		err := fmt.Errorf("%s is not paused, cannot resume", p.Name)
		glog.Error(err)
		return err
	}

	err := p.sandbox.Pause(false)
	if err != nil {
		glog.Error(err)
		return err
	}

	p.status.pod = S_POD_RUNNING
	return nil
}

func (p *Pod) Stop(graceful int) error {
	if !p.IsAlive() {
		err := fmt.Errorf("pod %s is not running for stop container", p.Name)
		glog.Error(err)
		return err
	}

	p.status.lock.RLock() // be aware of this lock, do not return before unlock
	wm := map[string]syscall.Signal{}
	tbs := []string{}
	for _, cs := range p.status.containers {
		if cs.IsRunning() {
			cdesc, _ := p.runtimeConfig.containers[cs.Id]
			wm[cs.Id] = StringToSignal(cdesc.StopSignal)
			tbs = append(tbs, cs.Id)
		}
	}
	p.status.lock.RUnlock() // be aware of this lock, do not return before unlock

	if len(wm) > 0 {
		r := p.sandbox.WaitProcess(true, tbs, graceful)
		if r != nil {
			for id, sig := range wm {
				glog.V(1).Infof("stopping %s: killing container %s with %d", p.Name, id, sig)
				err := p.sandbox.KillContainer(id, sig)
				if err != nil {
					glog.Error(err)
					delete(wm, id)
				}
			}
			for len(wm) > 0 {
				ex, ok := <- r
				if !ok {
					glog.Warningf("%s containers stop timeout or container waiting channel broken", p.Name)
					break
				}
				glog.V(1).Infof("container %s stopped", ex.Id)
				delete(wm, ex.Id)
			}
		} else {
			glog.Warningf("stopping %s: cannot wait containers %v", p.Name, tbs)
		}
	}

	rsp := p.sandbox.Shutdown()
	if rsp.IsSuccess() {
		glog.Infof("pod %s is stopped", p.Name)
		return nil
	}

	err := fmt.Errorf("pod %s stop exception: %s", p.Name, rsp.Message())
	glog.Error(err)
	return err
}

func (p *Pod) StopContainer(id string) error {
	if !p.IsAlive() {
		err := fmt.Errorf("pod %s is not running for stop container", p.Name)
		glog.Error(err)
		return err
	}

	p.status.lock.RLock()
	defer p.status.lock.RUnlock()

	cdesc, ok := p.runtimeConfig.containers[id]
	if !ok {
		err := fmt.Errorf("stop: can not find runtime config of container %s of pod %s", id, p.Name)
		glog.Error(err)
		return err
	}

	cs, ok := p.status.containers[id]
	if !ok {
		err := fmt.Errorf("stop: can not find status of container %s of pod %s", id, p.Name)
		glog.Error(err)
		return err
	}

	if !cs.Stop() {
		err := fmt.Errorf("stop: can stop container %s, current state: %d", id, cs.CurrentState())
		glog.Error(err)
		return err
	}

	return p.sandbox.KillContainer(id, StringToSignal(cdesc.StopSignal))
}

func (p *Pod) Rename(id, name string) {
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