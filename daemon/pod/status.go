package pod

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"

	apitypes "github.com/hyperhq/hyperd/types"
	runv "github.com/hyperhq/runv/api"
)

const (
	S_POD_NONE = iota // DEFAULT
	S_POD_CREATING    // vm context exist
	S_POD_RUNNING     // sandbox inited,
	S_POD_PAUSING
	S_POD_STOPPED     // vm stopped, no vm associated
)

const (
	S_CONTAINER_NONE = iota
	S_CONTAINER_CREATING
	S_CONTAINER_CREATED
	S_CONTAINER_RUNNING
	S_CONTAINER_STOPPING
)

const epocZero = time.Time{}

type ContainerStatus struct {
	Id         string
	State      int
	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time
	ExitCode   int

	sync.RWMutex
}

type ExecStatus struct {
	Id        string
	Container string
	Cmds      string
	Terminal  bool
	ExitCode  uint8
}

type PodStatus struct {
	pod         int
	containers  map[string]*ContainerStatus
	volumes     map[string]bool
	execs       map[string]*ExecStatus

	subscribers map[string][]chan<- runv.Result
	lock        *sync.RWMutex
}

func NewPodStatus() *PodStatus {
	return &PodStatus{
		containers: make(map[string]*ContainerStatus),
		volumes:    make(map[string]bool),
		lock:       &sync.RWMutex{},
	}
}

func NewContainerStatus(id string) *ContainerStatus {
	return &ContainerStatus{
		Id: id,
		State: S_CONTAINER_NONE,
		CreatedAt: epocZero,
		StartedAt: epocZero,
		FinishedAt: epocZero,
	}
}

func (cs *ContainerStatus) CurrentState() int {
	// do we need lock here?
	return cs.State
}

func (cs *ContainerStatus) Create() bool {
	cs.Lock()
	defer cs.Unlock()

	if cs.State != S_CONTAINER_NONE {
		glog.Errorf("%s: only NONE container could be create, current: %d", cs.Id, cs.State)
		return false
	}

	cs.State = S_CONTAINER_CREATING

	return true
}

func (cs *ContainerStatus) Created(t time.Time) bool {
	cs.Lock()
	defer cs.Unlock()
	if cs.State != S_CONTAINER_CREATING {
		glog.Errorf("%s: only CREATING container could be set to creatd, current: %d", cs.Id, cs.State)
		return false
	}

	cs.State = S_CONTAINER_CREATED
	cs.CreatedAt = t

	return true
}

func (cs *ContainerStatus) Start() bool {
	cs.Lock()
	defer cs.Unlock()

	if cs.State != S_CONTAINER_CREATED {
		glog.Errorf("%s: only CREATING container could be set to creatd, current: %d", cs.Id, cs.State)
		return false
	}

	cs.State = S_CONTAINER_RUNNING

	return true
}

func (cs *ContainerStatus) Running(t time.Time) bool {
	cs.Lock()
	defer cs.Unlock()

	if cs.State != S_CONTAINER_RUNNING {
		glog.Errorf("%s: only RUNNING container could set started time, current: %d", cs.Id, cs.State)
		return false
	}
	cs.StartedAt =  t
	return true
}

func (cs *ContainerStatus) Stop() bool {
	cs.Lock()
	defer cs.Unlock()

	if cs.State != S_CONTAINER_RUNNING {
		glog.Errorf("%s: only RUNNING container could be stopped, current: %d", cs.Id, cs.State)
		return false
	}
	cs.State = S_CONTAINER_STOPPING
	return true
}

func (cs *ContainerStatus) Stopped(t time.Time, exitCode int) bool {
	cs.Lock()
	defer cs.Unlock()

	cs.State = S_CONTAINER_CREATED
	if cs.State == S_CONTAINER_RUNNING || cs.State == S_CONTAINER_STOPPING {
		cs.FinishedAt = t
		cs.ExitCode = exitCode
		return true
	}
	return false
}

func (cs *ContainerStatus) UnexpectedStopped() bool {
	glog.Info("container %s stopped without return info", cs.Id)
	return cs.Stopped(time.Now(), 255)
}

func (cs *ContainerStatus) IsRunning() bool {
	cs.RLock()
	defer cs.RUnlock()

	return cs.State == S_CONTAINER_RUNNING
}

func (cs *ContainerStatus) IsStopped() bool {
	cs.RLock()
	defer cs.RUnlock()

	return cs.State == S_CONTAINER_CREATED
}

func (cs *ContainerStatus) IsAlive() bool {
	cs.RLock()
	defer cs.RUnlock()

	return cs.State == S_CONTAINER_RUNNING || cs.State == S_CONTAINER_CREATED || cs.State == S_CONTAINER_CREATING
}

func (ps *PodStatus) HasVolume(spec *apitypes.UserVolume) bool {
	_, ok := ps.volumes[spec.Name]
	return ok
}

func (ps *PodStatus) SubscribeVolume(name string, result chan<- runv.Result) {
	if o, ok := ps.subscribers[name]; ok {
		ps.subscribers[name] = append(o, result)
	} else {
		ps.subscribers[name] = []chan<- runv.Result{result}
	}
	if vs, ok := ps.volumes[name]; ok && vs {
		result <- runv.NewResultBase(name, true, "")
	}
}

func (ps *PodStatus) UnsubscribeVolume(name string, result chan<- runv.Result) {
	o, ok := ps.subscribers[name]
	if !ok {
		return
	}

	n := []chan<- runv.Result{}
	for i, c := range o {
		if result == c {
			n = append(n, o[i+1:]...)
			ps.subscribers[name] = n
			return
		}
		n = append(n, c)
	}
	ps.subscribers[name] = n
}

func (ps *PodStatus) VolumeDone(result runv.Result) {
	ps.volumes[result.ResultId()] = true
	if o, ok := ps.subscribers[result.ResultId()]; ok {
		for _, c := range o {
			c <- result
		}
	}
}

func (ps *PodStatus) NewContainer(id string) bool {
	ps.lock.Lock()
	defer ps.lock.Unlock()

	if _, ok := ps.containers[id]; ok {
		return false
	}

	ps.containers[id] = NewContainerStatus(id)
	return true
}

func (ps *PodStatus) SetExecStatus(execId string, code uint8) {
	exec, ok := ps.execs[execId]
	if ok {
		exec.ExitCode = code
	}
}

func (ps *PodStatus) AddExec(containerId, execId, cmds string, terminal bool) {
	ps.execs[execId] = &ExecStatus{
		Container: containerId,
		Id:        execId,
		Cmds:      cmds,
		Terminal:  terminal,
		ExitCode:  255,
	}
}

func (ps *PodStatus) DeleteExec(execId string) {
	delete(ps.execs, execId)
}

func (ps *PodStatus) CleanupExec() {
	ps.execs = make(map[string]*ExecStatus)
}

func (ps *PodStatus) GetExec(execId string) *ExecStatus {
	if exec, ok := ps.execs[execId]; ok {
		return exec
	}

	return nil
}

func (p *Pod) IsAlive() bool {
	p.status.lock.RLock()
	defer p.status.lock.RUnlock()
	return (p.status.pod == S_POD_RUNNING || p.status.pod == S_POD_CREATING) && p.sandbox != nil
}

func (p *Pod) ContainerIsAlive(id string) bool {
	p.status.lock.RLock()
	defer p.status.lock.RUnlock()

	status, ok := p.status.containers[id]
	if !ok {
		return false
	}

	return status.IsAlive()
}

func (p *Pod) ContainerIsStopped(id string) bool {
	p.status.lock.RLock()
	defer p.status.lock.RUnlock()

	status, ok := p.status.containers[id]
	if !ok {
		return false
	}

	return status.IsStopped()
}

func (p *Pod) ContainerIsRunning(id string) bool {
	p.status.lock.RLock()
	defer p.status.lock.RUnlock()

	status, ok := p.status.containers[id]
	if !ok {
		return false
	}

	return status.IsRunning()
}

func (p *Pod) SandboxName() string {
	if p.sandbox != nil {
		return p.sandbox.Id
	}
	return ""
}

func (p *Pod) SandboxStatusString() string {
	if p.sandbox != nil {
		if p.status.pod == S_POD_PAUSING {
			return "paused"
		}
		return "associated"
	}
	return ""
}

func (p *Pod) PodStatusString() string {
	p.status.lock.RLock()
	defer p.status.lock.RUnlock()

	sbn := ""
	if p.sandbox != nil {
		sbn = p.sandbox.Id
	}

	status := ""
	switch p.status.pod {
	case S_POD_NONE:
		status = "pending"
	case S_POD_CREATING:
		status = "pending"
	case S_POD_RUNNING:
		status = "running"
	case S_POD_STOPPED:
		status = "failed"
	case S_POD_PAUSING:
		status = "paused"
	}

	return strings.Join([]string{p.Name, sbn, status}, ":")
}

func (p *Pod) ContainerStatusString(id string) string {
	p.status.lock.RLock()
	defer p.status.lock.RUnlock()

	cs, ok := p.status.containers[id]
	if !ok {
		err := fmt.Errorf("can not find status of container %s of pod %s", id, p.Name)
		glog.Error(err)
		return ""
	}

	cdesc, ok := p.runtimeConfig.containers[id]
	if !ok {
		err := fmt.Errorf("can not find runtime config of container %s of pod %s", id, p.Name)
		glog.Error(err)
		return ""
	}

	status := ""
	switch cs.CurrentState() {
	case S_CONTAINER_NONE, S_CONTAINER_CREATING:
		status = "pending"
	case S_CONTAINER_RUNNING, S_CONTAINER_STOPPING:
		status = "running"
	case S_CONTAINER_CREATED:
		status = "pending"
		if !cs.FinishedAt.Equal(epocZero) {
			if cs.ExitCode == 0 {
				status = "succeeded"
			} else {
				status = "failed"
			}
		}
	}

	if p.status.pod == S_POD_PAUSING {
		status = "paused"
	}


	return strings.Join([]string{id, strings.TrimLeft(cdesc.Name, "/"), p.Name, status}, ":")
}
