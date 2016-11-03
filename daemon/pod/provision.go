package pod

import (
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/hyperhq/hyperd/lib/hlog"
	"github.com/hyperhq/hyperd/servicediscovery"
	apitypes "github.com/hyperhq/hyperd/types"
	"github.com/hyperhq/hyperd/utils"
	runv "github.com/hyperhq/runv/api"
	"github.com/hyperhq/runv/hypervisor"
)

var (
	ProvisionTimeout = 5 * time.Minute
)

func LoadXPod(factory *PodFactory, spec *apitypes.UserPod, sandboxId string) (*XPod, error) {
	p, err := newXPod(factory, spec)
	if err != nil {
		hlog.Log(ERROR, "failed to create pod from spec: %v", err)
		//remove spec from daemonDB
		//remove vm from daemonDB
		return nil, err
	}
	err = p.reserveNames(spec.Containers)
	if err != nil {
		return nil, err
	}
	err = p.reconnectSandbox(sandboxId)
	if err != nil {
		//remove vm from daemonDB
		return nil, err
	}

	err = p.initResources(spec, false)

	//resume logging
	if p.status == S_POD_RUNNING {
		for _, c := range p.containers {
			c.startLogging()
		}
	}

	// don't need to reserve name again, because this is load
	return p
}

func CreateXPod(factory *PodFactory, spec *apitypes.UserPod) (*XPod, error) {

	p, err := newXPod(factory, spec)
	if err != nil {
		return nil, err
	}
	err = p.reserveNames(spec.Containers)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			p.releaseNames(spec.Containers)
		}
	}()
	err = p.createSandbox(spec)
	if err != nil {
		return nil, err
	}

	err = p.initResources(spec, true)
	if err != nil {
		return nil, err
	}

	err = p.prepareResources()
	if err != nil {
		return nil, err
	}

	err = p.addResourcesToSandbox()
	if err != nil {
		return nil, err
	}

	//TODO: write the daemon db
	//daemon.WritePodAndContainers(pod.Id)

	// reserve again in case container is created
	err = p.reserveNames(spec.Containers)
	if err != nil {
		return nil, err
	}

	return p, nil
}

func newXPod(factory *PodFactory, spec *apitypes.UserPod) (*XPod, error) {
	if err := spec.MergePortmappings(); err != nil {
		hlog.Log(ERROR, "fail to merge the portmappings: %v", err)
		return nil, err
	}
	if err := spec.ReorganizeContainers(true); err != nil {
		hlog.Log(ERROR, err)
		return nil, err
	}
	factory.hosts = HostsCreator(spec.Id)
	factory.logCreator = initLogCreator(factory, spec)
	return &XPod{
		Name:         spec.Id,
		globalSpec:   spec.CloneGlobalPart(),
		containers:   make(map[string]*Container),
		volumes:      make(map[string]*Volume),
		portMappings: spec.Portmappings,
		labels:       spec.Labels,
		execs:        make(map[string]*Exec),
		resourceLock: &sync.Mutex{},
		statusLock:   &sync.RWMutex{},
		cleanChan:    make(chan bool, 1),
		factory:      factory,
	}, nil
}

// Start() means start a STOPPED pod.
func (p *XPod) Start() error {

	if p.status == S_POD_STOPPED {
		if err := p.createSandbox(p.globalSpec); err != nil {
			p.Log(ERROR, "failed to create sandbox for the stopped pod: %v", err)
			return err
		}

		if err := p.prepareResources(); err != nil {
			return err
		}

		if err := p.addResourcesToSandbox(); err != nil {
			return err
		}
	}

	if p.status == S_POD_RUNNING {
		if err := p.startAll(); err != nil {
			return err
		}
	} else {
		err := fmt.Errorf("%s, not in proper status and could not be started: %v", p.Name, p.status)
		p.Log(ERROR, err)
		return err
	}

	return nil
}

func (p *XPod) createSandbox(spec *apitypes.UserPod) error {
	sandbox, err := p.factory.runtime.StartSandbox(spec.Resource.Vcpu, spec.Resource.Memory)
	if err != nil {
		p.Log(ERROR, err)
		return err
	}

	config := &runv.SandboxConfig{
		Hostname: spec.Hostname,
		Dns:      spec.Dns,
		Neighbors: &runv.NeighborNetworks{
			InternalNetworks: spec.PortmappingWhiteLists.InternalNetworks,
			ExternalNetworks: spec.PortmappingWhiteLists.ExternalNetworks,
		},
	}

	p.status = S_POD_STARTING

	go p.waitVMInit()
	go p.waitVMStop()
	sandbox.InitSandbox(config)

	p.sandbox = sandbox
	return nil
}

func (p *XPod) reconnectSandbox(sandboxId string) error {
	var (
		sandbox *hypervisor.Vm
		err     error
	)

	if sandboxId != "" {
		sandbox, err = p.factory.runtime.AssociateSandbox(sandboxId)
		if err != nil {
			p.Log(ERROR, err)
			sandbox = nil
		}
	}

	if sandbox == nil {
		p.status = S_POD_STOPPED
		return err
	}

	p.status = S_POD_RUNNING
	p.sandbox = sandbox
	go p.waitVMStop()
	return nil
}

func (p *XPod) waitVMInit() {
	if p.status == S_POD_RUNNING {
		return
	}
	r := p.sandbox.WaitInit()
	p.Log(INFO, "sandbox init result: %#v", r)
	if r.IsSuccess() {
		p.statusLock.Lock()
		if p.status == S_POD_STARTING {
			p.status = S_POD_RUNNING
		}
		p.statusLock.Unlock()
	} else {
		p.statusLock.Lock()
		if p.sandbox != nil {
			go p.sandbox.Shutdown()
		}
		p.status = S_POD_STOPPING
		p.statusLock.Unlock()
	}
}

func (p *XPod) reserveNames(containers []*apitypes.UserContainer) error {
	var (
		err  error
		done = make([]*apitypes.UserContainer, 0, len(containers))
	)
	defer func() {
		if err != nil {
			p.releaseNames(done)
		}
	}()
	if err = p.factory.registry.ReservePod(p); err != nil {
		return err
	}
	for _, c := range containers {
		if err = p.factory.registry.ReserveContainer(c.Id, c.Name, p.Name); err != nil {
			p.Log(ERROR, err)
			return err
		}
		done = append(done, c)
	}
	return nil
}

func (p *XPod) releaseNames(containers []*apitypes.UserContainer) {
	for _, c := range containers {
		p.factory.registry.ReleaseContainer(c.Id, c.Name)
	}
	p.factory.registry.Release(p.Name)
}

// initResources() will create volumes, insert files etc. if needed.
// we can treat this function as an pre-processor of the spec
//
// If specify `allowCreate=true`, i.e. create rather than load, it will fill
// all the required fields, such as if an volume source is empty, this
// function will create the volume and fill the related fields.
//
// This function will do resource op and update the spec. and won't
// access sandbox.
func (p *XPod) initResources(spec *apitypes.UserPod, allowCreate bool) error {
	if sc := ParseServiceDiscovery(p.Name, spec); sc != nil {
		spec.Containers = append([]*apitypes.UserContainer{sc}, spec.Containers...)
	}

	for _, cspec := range spec.Containers {
		c, err := newContainer(p, cspec, allowCreate)
		if err != nil {
			return err
		}
		p.containers[c.Id()] = c

		vols := c.volumes()
		for _, vol := range vols {
			if _, ok := p.volumes[vol.Name]; ok {
				continue
			}
			p.volumes[vol.Name] = newVolume(p, vol)
		}
	}

	if len(spec.Interfaces) == 0 {
		spec.Interfaces = append(spec.Interfaces, &*apitypes.UserInterface{})
	}
	for _, nspec := range spec.Interfaces {
		inf := newInterface(p, nspec)
		p.interfaces[nspec.Ifname] = inf
	}

	p.services = spec.Services
	p.portMappings = spec.Portmappings

	return nil
}

// prepareResources() will allocate IP, generate service discovery config file etc.
// This apply for creating and restart a stopped pod.
func (p *XPod) prepareResources() error {
	var (
		err error
	)
	//generate /etc/hosts
	p.factory.hosts.Do()

	// gernerate service discovery config
	if len(p.services) > 0 {
		if err = servicediscovery.PrepareServices(p.services, p.Name); err != nil {
			p.Log(ERROR, "PrepareServices failed %v", err)
			return err
		}
	}

	defer func() {
		if err != nil {
			for _, inf := range p.interfaces {
				inf.cleanup()
			}
		}
	}()

	for _, inf := range p.interfaces {
		if err = inf.prepare(); err != nil {
			return err
		}
	}

	return nil
}

// addResourcesToSandbox() add resources to sandbox parallelly, it issues
// runV API parallelly to send the NIC, Vols, and Containers to sandbox
func (p *XPod) addResourcesToSandbox() error {
	p.Log(INFO, "adding resource to sandbox")
	future := utils.NewFutureSet()

	for ik, inf := range p.interfaces {
		future.Add(ik, inf.add)
	}

	for iv, vol := range p.volumes {
		future.Add(iv, vol.add)
	}

	for ic, c := range p.containers {
		future.Add(ic, c.addToSandbox)
	}

	if err := future.Wait(ProvisionTimeout); err != nil {
		p.Log(ERROR, "error during add resources to sandbox: %v", err)
		return err
	}
	return nil
}

func (p *XPod) startAll() error {
	p.Log(INFO, "start all containers")
	future := utils.NewFutureSet()

	for ic, c := range p.containers {
		future.Add(ic, c.start)
	}

	if err := future.Wait(ProvisionTimeout); err != nil {
		p.Log(ERROR, "error during start all containers: %v", err)
		return err
	}
	return nil
}

func (p *XPod) sandboxShareDir() string {
	if p.sandbox == nil {
		// the /dev/null is not a dir, then, can not create or open it
		return "/dev/null/no-such-dir"
	}
	return filepath.Join(hypervisor.BaseDir, p.sandbox.Id, hypervisor.ShareDirTag)
}
