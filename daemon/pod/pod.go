package pod

import (
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/docker/docker/daemon/logger"

	"github.com/hyperhq/hyperd/lib/hlog"
	apitypes "github.com/hyperhq/hyperd/types"
	"github.com/hyperhq/runv/hypervisor"
)

const (
	TRACE   = hlog.TRACE
	DEBUG   = hlog.DEBUG
	INFO    = hlog.INFO
	WARNING = hlog.WARNING
	ERROR   = hlog.ERROR
)

type PodState int32

const (
	S_POD_NONE PodState = iota // DEFAULT
	S_POD_STARTING    // vm context exist
	S_POD_RUNNING     // sandbox inited,
	S_POD_PAUSED
	S_POD_STOPPED     // vm stopped, no vm associated
	S_POD_STOPPING    // user initiates a stop/remove pod command
	S_POD_ERROR       // failed to stop/remove...
)

type XPod struct {
	Name         string

	globalSpec   *apitypes.UserPod

	// stateful resources:
	containers   map[string]*Container
	volumes      map[string]*Volume
	interfaces   map[string]*Interface
	services     []*apitypes.UserService
	portMappings []*apitypes.PortMapping
	labels       map[string]string
	resourceLock *sync.Mutex

	sandbox      *hypervisor.Vm
	factory      *PodFactory

	status       PodState
	execs        map[string]*Exec
	statusLock   *sync.RWMutex
	cleanChan    chan bool
}

func (p *XPod) LogPrefix() string {
	return fmt.Sprintf("Pod[%s] ", p.Name)
}

func (p *XPod) Log(level hlog.LogLevel, args ...interface{}) {
	hlog.HLog(level, p, 1, args...)
}

func (p *XPod) SandboxName() string {
	if p.sandbox != nil {
		return p.sandbox.Id
	}
	return ""
}

func (p *XPod) IsRunning() bool {
	p.statusLock.Lock()
	running := p.status == S_POD_RUNNING
	p.statusLock.Unlock()

	return running
}

func (p *XPod) IsAlive() bool {
	p.statusLock.Lock()
	alive := (p.status == S_POD_RUNNING) || (p.status == S_POD_STARTING)
	p.statusLock.Unlock()

	return alive
}

func (p *XPod) StatusString() string {
	var (
		status string
		sbn    string
	)
	p.statusLock.RLock()
	if p.sandbox != nil {
		sbn = p.sandbox.Id
	}
	switch p.status {
	case S_POD_NONE:
		status = "pending"
	case S_POD_STARTING:
		status = "pending"
	case S_POD_RUNNING:
		status = "running"
	case S_POD_STOPPED:
		status = "failed"
	case S_POD_PAUSED:
		status = "paused"
	case S_POD_STOPPING:
		status = "stopping"
	case S_POD_ERROR:
		status = "stopping"
	default:
	}
	p.statusLock.RUnlock()

	return strings.Join([]string{p.Name, sbn, status}, ":")
}

func (p *XPod) SandboxStatusString() string {
	var status string
	p.statusLock.RLock()
	if p.sandbox != nil {
		if p.status == S_POD_PAUSED {
			status = "paused"
		} else {
			status = "associated"
		}
	}
	p.statusLock.RUnlock()
	return status
}

func (p *XPod) ContainerCreate(c *apitypes.UserContainer) error {
	pc, err := newContainer(p, c, true)
	if err != nil {
		return err
	}

	p.containers[pc.SpecName()] = pc
	return nil
}

func (p *XPod) ContainerLogger(id string) logger.Logger {
	c, ok := p.containers[id]
	if ok {
		return c.getLogger()
	}
	p.Log(WARNING, "connot get container %s for logger", id)
	return nil
}

func (p *XPod) SetLabel(labels map[string]string, update bool) error {
	p.resourceLock.Lock()
	defer p.resourceLock.Unlock()

	if p.labels == nil {
		p.labels = make(map[string]string)
	}

	if !update {
		for k := range labels {
			if _, ok := p.labels[k]; ok {
				return fmt.Errorf("Can't update label %s without override flag", k)
			}
		}
	}

	for k, v := range labels {
		p.labels[k] = v
	}

	return nil
}

func (p *XPod) ContainerIds() []string {
	result := make([]string, 0, len(p.containers))
	for cid := range p.containers {
		result = append(result, cid)
	}
	return result
}

func (p *XPod) ContainerNames() []string {
	result := make([]string, 0, len(p.containers))
	for _, c := range p.containers {
		result = append(result, c.SpecName())
	}
	return result
}

func (p *XPod) ContainerIdsOf(ctype int32) []string {
	result := make([]string, 0, len(p.containers))
	for cid, c := range p.containers {
		if c.spec.Type == ctype {
			result = append(result, cid)
		}
	}
	return result
}

func (p *XPod) ContainerName2Id(name string) (string, bool) {
	if name == "" {
		return "", false
	}

	if _, ok := p.containers[name]; ok {
		return name, true
	}

	for _, c := range p.containers {
		if name == c.SpecName() || strings.HasPrefix(c.Id(), name) {
			return c.Id(), true
		}
	}

	return "", false
}

func (p *XPod) ContainerId2Name(id string) string {
	if c, ok := p.containers[id]; ok {
		return c.SpecName()
	}
	return ""
}

func (p *XPod) Attach(cid string, stdin io.ReadCloser, stdout io.WriteCloser, rsp chan<- error) error {
	if !p.IsAlive() {
		err := fmt.Errorf("only alive container could be attached, current %v", p.status)
		p.Log(ERROR, err)
		return err
	}
	c, ok := p.containers[cid]
	if !ok {
		err := fmt.Errorf("container %s not exist", cid)
		p.Log(ERROR, err)
		return err
	}
	if !c.IsAlive() {
		err := fmt.Errorf("container is not available: %v", c.CurrentState())
		c.Log(ERROR, err)
		return err
	}

	return c.attach(stdin, stdout, nil, rsp)
}
