package pod

import (
	"fmt"
	"syscall"

	"github.com/golang/glog"
	"github.com/docker/docker/pkg/signal"
)

func (p *Pod) Kill(id string, sig int64) error {
	return p.sandbox.KillContainer(id, syscall.Signal(sig))
}

func (p *Pod) ForceQuit() {
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

	sig, ok := signal.SignalMap[cdesc.StopSignal]
	if !ok {
		sig = syscall.SIGTERM
	}

	return p.sandbox.KillContainer(id, sig)
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