package pod

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/daemon/logger"
	"github.com/docker/docker/daemon/logger/jsonfilelog"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/docker/pkg/version"
	dockertypes "github.com/docker/engine-api/types"
	"github.com/docker/engine-api/types/container"
	"github.com/docker/engine-api/types/strslice"
	"github.com/golang/glog"

	"github.com/hyperhq/hyperd/servicediscovery"
	apitypes "github.com/hyperhq/hyperd/types"
	"github.com/hyperhq/hyperd/utils"
	runv "github.com/hyperhq/runv/api"
	"github.com/hyperhq/runv/hypervisor"
)

type PodConfig struct {
	sandboxConfig *runv.SandboxConfig
	interfaces    map[string]*runv.InterfaceDescription
	containers    map[string]*runv.ContainerDescription
	volumes       map[string]*runv.VolumeDescription
}

type LogStatus struct {
	Copier  *logger.Copier
	Driver  logger.Logger
	LogPath string
}

type ContainerSession struct {
	log LogStatus
}

type Pod struct {
	Name          string
	spec          *apitypes.UserPod

	sandbox       *hypervisor.Vm
	runtimeConfig *PodConfig
	status        *PodStatus
	sessions      map[string]*ContainerSession

	factory       *PodFactory
}

func NewPodConfig() *PodConfig {
	return &PodConfig{
		sandboxConfig: nil,
		interfaces:    make(map[string]*runv.InterfaceDescription),
		containers:    make(map[string]*runv.ContainerDescription),
		volumes:       make(map[string]*runv.VolumeDescription),
	}
}

func NewPod(factory *PodFactory, vm *hypervisor.Vm, spec *apitypes.UserPod) *Pod {
	p := &Pod{
		Name:          spec.Id,
		spec:          spec,
		sandbox:       vm,
		runtimeConfig: NewPodConfig(),
		status:        NewPodStatus(),
		sessions:      make(map[string]*ContainerSession),
		factory:       factory,
	}
	p.reorgSpec()
	p.status.pod = S_POD_NONE
	p.factory.hosts = HostsCreator(spec.Id)
	return p
}

func (pc *PodConfig) ConfigSandbox(spec *apitypes.UserPod) {
	pc.sandboxConfig = &runv.SandboxConfig{
		Hostname: spec.Hostname,
		Dns:      spec.Dns,
		Neighbors: &runv.NeighborNetworks{
			InternalNetworks: spec.PortmappingWhiteLists.InternalNetworks,
			ExternalNetworks: spec.PortmappingWhiteLists.ExternalNetworks,
		},
	}
}

func (p *Pod) CurrentState() int {
	p.status.lock.RLock()
	defer p.status.lock.RUnlock()
	return p.status.pod
}

//TODO: need a global index for containers
func (p *Pod) CreatePod() error {
	p.status.pod = S_POD_CREATING
	p.runtimeConfig.ConfigSandbox(p.spec)
	go p.waitVMInit()
 	p.sandbox.InitSandbox(p.runtimeConfig.sandboxConfig)

	if sc := ParseServiceDiscovery(p.Name, p.spec); sc != nil {
		p.spec.Containers = append([]*apitypes.UserContainer{sc}, p.spec.Containers...)
	}

	waitinglist := make(map[string]bool)
	type cresult struct{
		spec *apitypes.UserContainer
		err  error
	}
	result := make(chan *cresult)

	for _, c := range p.spec.Containers {
		waitinglist[c.Name] = true
		go func(spec *apitypes.UserContainer) {
			err := p.AddContainer(spec)
			result <- &cresult{ spec, err }
		}(c)
	}

	for len(waitinglist) > 0 {
		glog.V(1).Info("creating containers of pod %s: %#v", p.Name, waitinglist)
		select{
		case r := <-result:
			delete(waitinglist, r.spec.Name)
			if r.err != nil {
				err := fmt.Errorf("container %s (%s) of pod %s creation failed", r.spec.Name, r.spec.Image, p.Name)
				glog.Errorf(err.Error())
				return err
			}
			glog.Info("container %s (%s) of pod %s created")
		case <-time.After(2*time.Minute): // 2 minutes after the previous container created
			err := fmt.Errorf("containers creation timeout: %#v", waitinglist)
			glog.Errorf(err.Error())
			return err
		}
	}
	glog.Info("pod %s created", p.Name)
	return nil
}

func (p *Pod) StartPod() (success []string, failed []string, err error) {
	success = []string{}
	failed = []string{}
	for _, c := range p.status.containers {
		if er := p.StartContainer(c.Id); er != nil {
			glog.Errorf("failed to start container %s: %v", c.Id, er)
			failed = append(failed, c.Id)
		} else {
			success = append(success, c.Id)
		}
	}
	if len(failed) > 0 {
		err = fmt.Errorf("the following containers in pod %s does not start successfully: %v", p.Name, failed)
	}
	return success, failed, err
}

func (p *Pod) ContainerIds() []string {
	p.status.lock.RLock()
	defer p.status.lock.RUnlock()

	result := []string{}
	for _, c := range p.status.containers {
		result = append(result, c.Id)
	}

	return result
}

func (p *Pod) ContainerIdsOf(ctype int32) []string {
	p.status.lock.RLock()
	defer p.status.lock.RUnlock()

	result := []string{}
	for _, c := range p.spec.Containers {
		if c.Id != "" && c.Type == ctype {
			result = append(result, c.Id)
		}
	}

	return result
}

func (p *Pod) ContainerNames() []string {
	p.status.lock.RLock()
	defer p.status.lock.RUnlock()

	result := []string{}
	for _, c := range p.spec.Containers {
		result = append(result, c.Name)
	}

	return result
}

func (p *Pod) AddContainer(spec *apitypes.UserContainer) error {

	var (
		cjson  *dockertypes.ContainerJSON
		err    error
		loaded bool
	)

	if spec.Name == "" {
		//TODO: Generate Name
	}

	if spec.Id != "" {
		cjson = p.tryLoadContainer(spec)
		// if label tagged this is a new container, should set `loaded` false
		loaded = true
		p.status.NewContainer(spec.Id)
		p.status.containers[spec.Id].Create()
	}

	if cjson == nil {
		cjson, err = p.createContainer(spec)
		if err != nil {
			return err
		}
		p.status.NewContainer(spec.Id)
		p.status.containers[spec.Id].Create()
	}

	desc, err := p.describeContainer(spec, cjson)
	if err != nil {
		return err
	}
	desc.Initialize = !loaded

	if err = p.mountContainerAndVolumes(spec, desc, cjson, loaded); err != nil {
		return err
	}

	// At last
	ctime, _ := utils.ParseTimeString(cjson.Created)

	p.status.lock.Lock()
	defer p.status.lock.Unlock()

	p.runtimeConfig.containers[spec.Id] = desc
	p.sessions[spec.Id] = &ContainerSession{}
	p.status.containers[spec.Id].Created(ctime)
	p.factory.registry.PutContainerID(spec.Id, p.Name)
	return nil
}

func (p *Pod) StartContainer(id string) error {
	cs, ok := p.status.containers[id]
	if !ok {
		err := fmt.Errorf("starting a non-created container %s", id)
		glog.Errorf(err.Error())
		return err
	}

	desc, ok := p.runtimeConfig.containers[id]
	if !ok {
		err := fmt.Errorf("container %s do not have runtime config", id)
		glog.Errorf(err.Error())
		return err
	}

	ok = cs.Start()
	if !ok {
		return fmt.Errorf("start from incorrect state: %d", cs.CurrentState())
	}

	spec := p.spec.LookupContainer(id)
	if spec != nil && spec.Type == apitypes.UserContainer_REGULAR {
		p.startLogging(desc)
	}

	go p.waitContainer(cs)

	p.sandbox.StartContainer(id)

	return nil
}

func (p *Pod) WaitContainerFinish(id string) {
	cs, ok := p.status.containers[id]
	if !ok {
		err := fmt.Errorf("starting a non-created container %s", id)
		glog.Errorf(err.Error())
		return
	}

	p.waitContainer(cs)
}

func (p *Pod) AddVolume(spec *apitypes.UserVolume) {
	p.status.lock.Lock()
	defer p.status.lock.Unlock()

	if p.status.HasVolume(spec) {
		return
	}

	p.status[spec.Name] = false

	go p.mountVolume(spec)
}

func (p *Pod) SetLabel(labels map[string]string, update bool) error {
	p.status.lock.Lock()
	defer p.status.lock.Unlock()

	if p.spec.Labels == nil {
		p.spec.Labels = make(map[string]string)
	}

	if !update {
		for k := range labels {
			if _, ok := p.spec.Labels[k]; ok {
				return fmt.Errorf("Can't update label %s without override flag", k)
			}
		}
	}

	for k, v := range labels {
		p.spec.Labels[k] = v
	}

	return nil
}

func (p *Pod) Attach(tty *hypervisor.TtyIO, id string) error {
	if p.sandbox == nil {
		err := fmt.Errorf("no sandbox is ready for attach to %s", id)
		glog.Error(err)
		return err
	}

	if tty.Stdout != nil {
		if desc, ok := p.runtimeConfig.containers[id]; ok && !desc.Tty{
			tty.Stderr = stdcopy.NewStdWriter(tty.Stdout, stdcopy.Stderr)
			tty.Stdout = stdcopy.NewStdWriter(tty.Stdout, stdcopy.Stdout)
		}
	}

	return p.sandbox.Attach(tty, id, nil)
}

func (p *Pod) ContainerName2Id(name string) (string, bool) {
	if name == "" {
		return "", false
	}

	for _, c := range p.spec.Containers {
		if name == c.Name || name == c.Id {
			return c.Id, true
		} else if strings.HasPrefix(c.Id, name) {
			return c.Id, true
		}
	}

	return "", false
}

func (p *Pod) ContainerLogger(id string) logger.Logger {
	session, ok := p.sessions[id]
	if ok {
		return session.log.Driver
	}

	desc, ok := p.runtimeConfig.containers[id]
	if !ok {
		return nil
	}

	p.getLogger(desc)

	session, ok = p.sessions[id]
	if ok {
		return session.log.Driver
	}
	return nil
}

func (p *Pod) ContainerIsAlive(id string) bool {
	p.status.lock.RLock()
	defer p.status.lock.RUnlock()

	status, ok := p.status.containers[id]
	if !ok {
		return false
	}

	s := status.CurrentState()

	return s == S_CONTAINER_RUNNING || s == S_CONTAINER_CREATED || s == S_CONTAINER_CREATING
}

func (p *Pod) ContainerHasTty(id string) bool {
	desc, ok := p.runtimeConfig.containers[id]
	if !ok {
		return false
	}
	return desc.Tty
}

func (p *Pod) reorgSpec() {
	volumes := make(map[string]*apitypes.UserVolume)
	files := make(map[string]*apitypes.UserFile)
	for _, vol := range p.spec.Volumes {
		volumes[vol.Name] = vol
	}
	for _, file := range p.spec.Files {
		files[file.Name] = file
	}

	for idx, c := range p.spec.Containers {

		if c.Name == "" {
			all := strings.SplitN(c.Image, ":", 2)
			repo := all[0]
			segs := strings.Split(repo, "/")
			img := segs[len(segs)-1]
			if !utils.IsDNSLabel(img) {
				img = ""
			}

			c.Name = fmt.Sprintf("%s-%s-%d", p.Name, img, idx)
		}

		for _, vol := range c.Volumes {
			if vol.Detail != nil {
				continue
			}
			if v, ok := volumes[vol.Volume]; !ok {
				glog.Errorf("volume %s of container %s do not have specification", vol.Volume, c.Name)
				continue
			} else {
				vol.Detail = v
			}

		}
		for _, file := range c.Files {
			if file.Detail != nil {
				continue
			}
			if f, ok := files[file.Filename]; !ok {
				glog.Errorf("file %s of container %s do not have specification", file.Filename, c.Name)
				continue
			} else {
				file.Detail = f
			}

		}
	}
}

func (p *Pod) initLogCreator() {
	if p.spec.Log.Type == "" {
		p.spec.Log.Type = p.factory.logCfg.Type
		p.spec.Log.Config = p.factory.logCfg.Config
	}

	if p.spec.Log.Type == "none" {
		return
	}

	var (
		creator logger.Creator
		err     error
	)

	if err = logger.ValidateLogOpts(p.spec.Log.Type, p.factory.logCfg.Config); err != nil {
		glog.Errorf("invalid log options for pod %s. type: %s; options: %#v", p.Name, p.spec.Log.Type, p.factory.logCfg.Config)
		return
	}
	creator, err = logger.GetLogDriver(p.spec.Log.Type)
	if err != nil {
		glog.Errorf("cannot create logCreator for pod %s. type: %s; err: %v", p.Name, p.spec.Log.Type, err)
		return
	}
	glog.V(1).Infof("configuring log driver [%s] for %s", p.spec.Log.Type, p.Name)

	p.factory.logCreator = creator
	return
}

func (p *Pod) tryLoadContainer(spec *apitypes.UserContainer) *dockertypes.ContainerJSON {
	if spec.Id == "" {
		return nil
	}

	glog.V(3).Infof("Loading container %s of pod %s", spec.Id, p.Name)
	if r, err := p.factory.engine.ContainerInspect(spec.Id, false, version.Version("1.21")); err != nil {
		rsp, ok := r.(*dockertypes.ContainerJSON)
		if !ok {
			glog.Warningf("fail to load container %s for pod %s: %s", spec.Id, p.Name, string(r))
			return nil
		}

		n := strings.TrimLeft(rsp.Name, "/")
		if spec.Name != rsp.Name {
			glog.Warningf("name mismatch of loaded container %s (%s) for pod %s: loaded is %s", spec.Id, spec.Name, p.Name, n)
			spec.Id = ""
			return nil
		}
		glog.V(1).Infof("Found exist container %s (%s), pod: %s", spec.Id, spec.Name, p.Name)
		return rsp
	}

	return nil
}

func (p *Pod) createContainer(spec *apitypes.UserContainer) (*dockertypes.ContainerJSON, error) {
	var (
		ok  bool
		err error
		ccs dockertypes.ContainerCreateResponse
		rsp *dockertypes.ContainerJSON
		r   interface{}
	)

	config := &container.Config{
		Image:           spec.Image,
		Cmd:             strslice.New(spec.Command...),
		NetworkDisabled: true,
	}

	if len(spec.Entrypoint) != 0 {
		config.Entrypoint = strslice.New(spec.Entrypoint...)
	}

	if len(spec.Envs) != 0 {
		envs := []string{}
		for _, env := range spec.Envs {
			envs = append(envs, env.Env+"="+env.Value)
		}
		config.Env = envs
	}

	ccs, err = p.factory.engine.ContainerCreate(dockertypes.ContainerCreateConfig{
		Name:   spec.Name,
		Config: config,
	})

	if err != nil {
		return nil, err
	}

	glog.Infof("create container %s", ccs.ID)
	if r, err = p.factory.engine.ContainerInspect(ccs.ID, false, version.Version("1.21")); err != nil {
		return nil, err
	}

	if rsp, ok = r.(*dockertypes.ContainerJSON); !ok {
		err = fmt.Errorf("fail to unpack container json response for %s of %s", spec.Name, p.Name)
		return nil, err
	}

	spec.Id = ccs.ID
	return rsp, nil

}

func (p *Pod) describeContainer(spec *apitypes.UserContainer, cjson *dockertypes.ContainerJSON) (*runv.ContainerDescription, error) {

	glog.V(3).Infof("container info config %v, Cmd %v, Args %v", cjson.Config, cjson.Config.Cmd.Slice(), cjson.Args)

	if spec.Image == "" {
		spec.Image = cjson.Config.Image
	}
	glog.Infof("container name %s, image %s", spec.Name, spec.Image)

	mountId, err := GetMountIdByContainer(p.factory.sd.Type(), spec.Id)
	if err != nil {
		estr := fmt.Sprintf("Cannot find mountID for container %s : %s", spec.Id, err)
		glog.Error(estr)
		return nil, errors.New(estr)
	}

	container := &runv.ContainerDescription{
		Id: spec.Id,

		Name:  cjson.Name, // will have a "/"
		Image: cjson.Image,

		Labels:        spec.Labels,
		Tty:           spec.Tty,
		RestartPolicy: spec.RestartPolicy,

		RootVolume: &runv.VolumeDescription{},
		MountId:    mountId,
		RootPath:   "rootfs",

		Envs:    make(map[string]string),
		Workdir: cjson.Config.WorkingDir,
		Path:    cjson.Path,
		Args:    cjson.Args,
	}

	if cjson.Config.User != "" {
		container.UGI = &runv.UserGroupInfo{
			User: cjson.Config.User,
		}
	}

	for _, v := range cjson.Config.Env {
		pair := strings.SplitN(v, "=", 2)
		if len(pair) == 2 {
			container.Envs[pair[0]] = pair[1]
		} else if len(pair) == 1 {
			container.Envs[pair[0]] = ""
		}
	}

	glog.V(3).Infof("Container Info is \n%#v", container)

	return container, nil
}

func (p *Pod) configEtcHosts(spec *apitypes.UserContainer) *runv.VolumeReference {
	var (
		hostsVolumeName = "etchosts-volume"
		hostsVolumePath = ""
		hostsPath       = "/etc/hosts"
	)

	for _, v := range spec.Volumes {
		if v.Path == hostsPath {
			return nil
		}
	}

	for _, f := range spec.Files {
		if f.Path == hostsPath {
			return nil
		}
	}

	p.factory.hosts.Do()
	_, hostsVolumePath = HostsPath(p.Name)

	vol := &apitypes.UserVolume{
		Name:   hostsVolumeName,
		Source: hostsVolumePath,
		Format: "vfs",
		Fstype: "dir",
	}

	ref := &apitypes.UserVolumeReference{
		Path:     hostsPath,
		Volume:   hostsVolumeName,
		ReadOnly: false,
		Detail:   vol,
	}

	spec.Volumes = append(spec.Volumes, ref)
	p.AddVolume(vol)

	return &runv.VolumeReference{
		Path:     hostsPath,
		Name:     hostsVolumeName,
		ReadOnly: false,
	}
}

/***
  PrepareDNS() Set the resolv.conf of host to each container, except the following cases:

  - if the pod has a `dns` field with values, the pod will follow the dns setup, and daemon
    won't insert resolv.conf file into any containers
  - if the pod has a `file` which source is uri "file:///etc/resolv.conf", this mean the user
    will handle this file by himself/herself, daemon won't touch the dns setting even if the file
    is not referenced by any containers. This could be a method to prevent the daemon from unwanted
    setting the dns configuration
  - if a container has a file config in the pod spec with `/etc/resolv.conf` as target `path`,
    then this container won't be set as the file from hosts. Then a user can specify the content
    of the file.

*/
func (p *Pod) configDNS(spec *apitypes.UserContainer) {
	var (
		resolvconf = "/etc/resolv.conf"
		fileId     = p.Name + "-resolvconf"
	)

	if len(p.spec.Dns) > 0 {
		glog.V(1).Info("Already has DNS config, bypass DNS insert")
		return
	}

	if stat, e := os.Stat(resolvconf); e != nil || !stat.Mode().IsRegular() {
		glog.V(1).Info("Host resolv.conf does not exist or not a regular file, do not insert DNS conf")
		return
	}

	for _, src := range p.spec.Files {
		if src.Uri == "file:///etc/resolv.conf" {
			glog.V(1).Info("Already has resolv.conf configured, bypass DNS insert")
			return
		}
	}

	for _, vol := range spec.Volumes {
		if vol.Path == resolvconf {
			glog.V(1).Info("Already has resolv.conf configured, bypass DNS insert")
			return
		}
	}

	for _, ref := range spec.Files {
		if ref.Path == resolvconf || (ref.Path+ref.Filename) == resolvconf || ref.Detail.Uri == "file:///etc/resolv.conf" {
			glog.V(1).Info("Already has resolv.conf configured, bypass DNS insert")
			return
		}
	}

	spec.Files = append(spec.Files, &apitypes.UserFileReference{
		Path:     resolvconf,
		Filename: fileId,
		Perm:     "0644",
		Detail: &apitypes.UserFile{
			Name:     fileId,
			Encoding: "raw",
			Uri:      "file://" + resolvconf,
		},
	})
}

func (p *Pod) volumesInContainer(spec *apitypes.UserContainer, cjson *dockertypes.ContainerJSON) map[string]*runv.VolumeReference {

	var (
		existed = make(map[string]bool)
		refs    = make(map[string]*runv.VolumeReference)
	)
	for _, vol := range spec.Volumes {
		if vol.Detail != nil {
			p.AddVolume(vol.Detail)
		}
		existed[vol.Path] = true
		refs[vol.Volume] = &runv.VolumeReference{
			Path:     vol.Path,
			Name:     vol.Volume,
			ReadOnly: vol.ReadOnly,
		}
	}

	if cjson == nil {
		return refs
	}

	for tgt := range cjson.Config.Volumes {
		if _, ok := existed[tgt]; ok {
			continue
		}

		n := spec.Id + strings.Replace(tgt, "/", "_", -1)
		v := apitypes.UserVolume{
			Name:   n,
			Source: "",
		}
		r := apitypes.UserVolumeReference{
			Volume:   n,
			Path:     tgt,
			ReadOnly: false,
			Detail:   v,
		}

		p.AddVolume(v)
		spec.Volumes = append(spec.Volumes, r)
		refs[n] = &runv.VolumeReference{
			Path:     tgt,
			Name:     n,
			ReadOnly: false,
		}
	}

	return refs
}

func (p *Pod) waitVolumes(spec *apitypes.UserContainer, max_wait int) runv.Result {

	var timeout chan<- time.Time

	waitMap := make(map[string]bool)
	result := make(chan runv.Result, len(spec.Volumes))
	for _, vol := range spec.Volumes {
		p.status.SubscribeVolume(vol.Volume, result)
		waitMap[vol.Volume] = true
	}

	if max_wait < 0 {
		timeout = make(chan time.Time)
	} else {
		timeout = time.After(time.Duration(int64(time.Second) * max_wait))
	}
	for len(waitMap) > 0 {
		select {
		case r := <-result:
			if _, ok := waitMap[r.ResultId()]; !ok {
				glog.Warningf("got response from volume %s, but it is not waited by %s", r.ResultId(), spec.Id)
				continue
			}
			if !r.IsSuccess() {
				glog.Errorf("fail to insert volume %s for container %s: %s", r.ResultId(), spec.Id, r.Message())
				return runv.NewResultBase(spec.Id, false, "add volume failed: "+r.ResultId())
			}
			delete(waitMap, r.ResultId())
		case <-timeout:
			glog.Errorf("container %s: volume waiting timeout, remains")
			return runv.NewResultBase(spec.Id, false, "add volume timeout")
		}
	}

	return runv.NewResultBase(spec.Id, true, "")
}

func (p *Pod) prepareRootVolume(spec *apitypes.UserContainer, container *runv.ContainerDescription) error {
	var (
		sharedDir = path.Join(hypervisor.BaseDir, p.Name, hypervisor.ShareDirTag)
	)

	glog.Infof("container ID: %s, mountId %s\n", spec.Id, container.MountId)
	v, err := p.factory.sd.PrepareContainer(container.MountId, sharedDir)
	if err != nil {
		return err
	}

	container.RootVolume = v
	return nil

}

func (p *Pod) processInjectFiles(spec *apitypes.UserContainer) error {
	var (
		sharedDir = path.Join(hypervisor.BaseDir, p.Name, hypervisor.ShareDirTag)
	)

	for _, f := range spec.Files {
		targetPath := f.Path
		if strings.HasSuffix(targetPath, "/") {
			targetPath = targetPath + f.Filename
		}

		if f.Detail == nil {
			continue
		}

		file := f.Detail
		var src io.Reader

		if file.Uri != "" {
			urisrc, err := utils.UriReader(file.Uri)
			if err != nil {
				return err
			}
			defer urisrc.Close()
			src = urisrc
		} else {
			src = strings.NewReader(file.Content)
		}

		switch file.Encoding {
		case "base64":
			src = base64.NewDecoder(base64.StdEncoding, src)
		default:
		}

		err := p.factory.sd.InjectFile(src, spec.Id, targetPath, sharedDir,
			utils.PermInt(f.Perm), utils.UidInt(f.User), utils.UidInt(f.Group))
		if err != nil {
			glog.Error("got error when inject files ", err.Error())
			return err
		}
	}

	return nil
}

func (p *Pod) mountContainerAndVolumes(spec *apitypes.UserContainer, desc *runv.ContainerDescription, cjson *dockertypes.ContainerJSON, loaded bool) error {

	desc.Volumes = p.volumesInContainer(spec, cjson)

	// should be
	//   - called later than parse volumes, guarantee the file is not described in container
	//   - before wait volumes, need wait this insert as well
	// and this func call addVolume for the /etc/hosts vol
	if hv := p.configEtcHosts(spec); hv != nil {
		desc.Volumes[hv.Name] = hv
	}

	if err := p.prepareRootVolume(spec, desc); err != nil {
		return err
	}

	if spec.Type == apitypes.UserContainer_SERVICE {
		if err := servicediscovery.PrepareServices(p.spec.Services, p.Name); err != nil {
			glog.Errorf("PrepareServices failed %s", err.Error())
			return err
		}
	}

	if !loaded {
		p.configDNS(spec)

		if err := p.processInjectFiles(spec); err != nil {
			return err
		}
	}

	r := p.waitVolumes(spec, -1)
	if !r.IsSuccess() {
		return fmt.Errorf("failed to add volumes: %#v", r)
	}

	r = p.sandbox.AddContainer(desc)
	if !r.IsSuccess() {
		return fmt.Errorf("failed to add container: %#v", r)
	}

	return nil
}

func (p *Pod) getLogger(desc *runv.ContainerDescription) {
	if p.factory.logCreator == nil {
		return
	}

	session, ok := p.sessions[desc.Id]
	if !ok {
		glog.Errorf("no session for logger, container %s", desc.Id)
		return
	}

	status, ok := p.status.containers[desc.Id]
	if !ok {
		glog.Errorf("no status for logger, container %s", desc.Id)
		return
	}

	if session.log.Driver != nil {
		return
	}

	prefix := p.factory.logCfg.PathPrefix
	if p.factory.logCfg.PodIdInPath {
		prefix = filepath.Join(prefix, p.Name)
	}
	if err := os.MkdirAll(prefix, os.FileMode(0755)); err != nil {
		glog.Errorf("cannot create container log dir %s: %v", prefix, err)
		return
	}

	ctx := logger.Context{
		Config:              p.factory.logCfg.Config,
		ContainerID:         desc.Id,
		ContainerName:       desc.Name,
		ContainerImageName:  desc.Image,
		ContainerCreated:    status.CreatedAt,
		ContainerEntrypoint: desc.Path,
		ContainerArgs:       desc.Args,
		ContainerImageID:    desc.Image,
	}

	if p.factory.logCfg.Type == jsonfilelog.Name {
		ctx.LogPath = filepath.Join(prefix, fmt.Sprintf("%s-json.log", desc.Id))
		glog.V(1).Info("configure container log to ", ctx.LogPath)
	}

	driver, err := p.factory.logCreator(ctx)
	if err != nil {
		return
	}
	session.log.Driver = driver
	glog.V(1).Infof("configured logger for %s/%s (%s)", p.Name, desc.Id, desc.Name)

	return

}

func (p *Pod) startLogging(desc *runv.ContainerDescription) {

	var err error

	p.getLogger(desc)

	session, ok := p.sessions[desc.Id]
	if !ok {
		glog.Errorf("no session for logger, container %s", desc.Id)
		return
	}

	if session.log.Driver == nil {
		return
	}

	var stdout, stderr io.Reader

	if stdout, stderr, err = p.sandbox.GetLogOutput(desc.Id, nil); err != nil {
		return
	}
	session.log.Copier = logger.NewCopier(desc.Id, map[string]io.Reader{"stdout": stdout, "stderr": stderr}, session.log.Driver)
	session.log.Copier.Run()

	if jl, ok := session.log.Driver.(*jsonfilelog.JSONFileLogger); ok {
		session.log.LogPath = jl.LogPath()
	}

	return
}

func (p *Pod) createVolume(spec *apitypes.UserVolume) error {
	if spec.Source != "" {
		return nil
	}

	return p.factory.sd.CreateVolume(p.Name, spec)
}

func (p *Pod) mountVolume(spec *apitypes.UserVolume) {

	var (
		created bool
		err     error
		r       runv.Result
		vol     *runv.VolumeDescription
	)

	if spec.Source == "" {
		err = p.createVolume(spec)
		created = true
	}

	if err == nil {
		sharedDir := path.Join(hypervisor.BaseDir, p.sandbox.Id, hypervisor.ShareDirTag)
		vol, err = ProbeExistingVolume(spec, sharedDir)
	}

	if err != nil {
		r = runv.NewResultBase(spec.Name, false, err.Error())
	} else {
		// Here we mark all created volume as DockerVolume, i.e. all the created empty volume
		// will be initialize by hyperstart
		vol.DockerVolume = created
		// This op will block until add volume finish
		r = p.sandbox.AddVolume(vol)
	}

	p.status.lock.Lock()
	defer p.status.lock.Unlock()

	if r.IsSuccess() {
		p.runtimeConfig.volumes[spec.Name] = vol
	}
	p.status.VolumeDone(r)
}

func (p *Pod) waitContainer(cs *ContainerStatus) {

	var firstStop bool

	result := p.sandbox.WaitProcess(true, []string{cs.Id}, -1)
	if result == nil {
		firstStop = cs.UnexpectedStopped()
	} else {
		r, ok := <-result
		if !ok {
			firstStop = cs.UnexpectedStopped()
		} else {
			firstStop = cs.Stopped(r.FinishedAt, r.Code)
		}
	}

	if firstStop {
		session, ok := p.sessions[cs.Id]
		if ok && session.log.Driver != nil {
			session.log.Driver.Close()
		}
	}
}

func (p *Pod) cleanupContainer() {

}

func (p *Pod) waitVMInit() {
	if p.status.pod == S_POD_RUNNING {
		return
	}
	r := p.sandbox.WaitInit()
	glog.Info("sandbox init result: %#v", r)
	if r.IsSuccess() {
		p.status.lock.Lock()
		p.status.pod = S_POD_RUNNING
		p.status.lock.Unlock()
	} else {
		if p.sandbox != nil {
			p.sandbox.Shutdown()
			p.sandbox = nil
		}
		p.status.lock.Lock()
		//TODO: daemon.PodStopped(mypod.Id)
		p.status.pod = S_POD_STOPPED
		p.status.lock.Unlock()
	}
}

func (p *Pod) waitVM() {
	if p.status.pod == S_POD_STOPPED {
		return
	}
	_, _ = <- p.sandbox.WaitVm(-1)
	glog.Info("got vm exit event")
	p.status.lock.Lock()
	//TODO: daemon.PodStopped(mypod.Id)
	p.status.pod = S_POD_STOPPED
	p.status.lock.Unlock()
}
