package pod

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/daemon/logger"
	"github.com/docker/docker/daemon/logger/jsonfilelog"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/docker/pkg/version"
	dockertypes "github.com/docker/engine-api/types"
	"github.com/docker/engine-api/types/container"
	"github.com/docker/engine-api/types/strslice"

	"github.com/hyperhq/hyperd/lib/hlog"
	apitypes "github.com/hyperhq/hyperd/types"
	"github.com/hyperhq/hyperd/utils"
	runv "github.com/hyperhq/runv/api"
	"github.com/hyperhq/runv/hypervisor"
	runvtypes "github.com/hyperhq/runv/hypervisor/types"
)

const epocZero = time.Time{}
type ContainerState int32
const (
	S_CONTAINER_NONE ContainerState = iota
	S_CONTAINER_CREATING
	S_CONTAINER_CREATED
	S_CONTAINER_RUNNING
	S_CONTAINER_STOPPING
)

type ContainerStatus struct {
	State      ContainerState
	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time
	ExitCode   int

	sync.RWMutex
}

// A Container is run inside a Pod. It could be created as a member of a pod,
// and belongs to the pod until it is removed.
type Container struct {
	p *XPod

	spec     *apitypes.UserContainer
	descript *runv.ContainerDescription
	status   *ContainerStatus

	logger LogStatus
}

func newContainerStatus() *ContainerStatus {
	return &ContainerStatus{
		State: S_CONTAINER_NONE,
		CreatedAt: epocZero,
		StartedAt: epocZero,
		FinishedAt: epocZero,
	}
}

func newContainer(p *XPod, spec *apitypes.UserContainer, create bool) (*Container, error) {
	c := &Container{
		p:      p,
		spec:   spec,
		status: newContainerStatus(),
	}
	if err := c.init(create); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Container) LogPrefix() string {
	return fmt.Sprintf("%sCon[%s(%s)] ", c.p.LogPrefix(), c.Id(), c.SpecName())
}

func (c *Container) Log(level hlog.LogLevel, args ...interface{}) {
	hlog.HLog(level, c, 1, args...)
}

// Container Info:
func (c *Container) Id() string {
	return c.spec.Id
}

func (c *Container) SpecName() string {
	return c.spec.Name
}

func (c *Container) RuntimeName() string {
	if c.descript != nil {
		return c.descript.Name
	}
	return ""
}

func (c *Container) hasTty() bool {
	return c.spec.Tty
}

// Container life cycle operations:
func (c *Container) Add() error {
	return nil
}

func (c *Container) start() error {
	if err := c.status.Start(); err != nil {
		c.Log(ERROR, err)
		return err
	}

	if c.spec.Type == apitypes.UserContainer_REGULAR {
		c.startLogging()
	}

	go c.waitFinish(-1)

	c.Log(INFO, "start container")
	c.p.sandbox.StartContainer(c.Id())

	return nil
}

func (c *Container) Remove() error {
	return nil
}

// Container operations:

func (c *Container) attach( stdin io.ReadCloser, stdout io.WriteCloser, winsize *hypervisor.WindowSize, rsp chan<- error) error {
	if c.p.sandbox == nil || c.descript == nil {
		err := fmt.Errorf("container not ready for attach")
		c.Log(ERROR, err)
		return err
	}

	tty := &hypervisor.TtyIO{
		Stdin:    stdin,
		Stdout:   stdout,
		Callback: make(chan *runvtypes.VmResponse, 1),
	}

	if stdout != nil {
		if !c.hasTty() {
			tty.Stderr = stdcopy.NewStdWriter(stdout, stdcopy.Stderr)
			tty.Stdout = stdcopy.NewStdWriter(stdout, stdcopy.Stdout)
		}
	}

	if rsp != nil {
		go func() {
			rsp <- tty.WaitForFinish()
		}()
	}

	return c.p.sandbox.Attach(tty, c.Id(), winsize)
}

// Container status
func (c *Container) CurrentState() ContainerState {
	c.status.RLock()
	current := c.status.State
	c.status.RUnlock()

	return current
}

func (c *Container) IsAlive() bool {
	c.status.RLock()
	alive := (c.status.State == S_CONTAINER_RUNNING || c.status.State == S_CONTAINER_CREATED || c.status.State == S_CONTAINER_CREATING)
	c.status.RUnlock()

	return alive
}

func (c *Container) IsRunning() bool {
	c.status.RLock()
	running := c.status.State == S_CONTAINER_RUNNING
	c.status.RUnlock()

	return running
}

func (c *Container) IsStopped() bool {
	c.status.RLock()
	stopped := c.status.State == S_CONTAINER_CREATED
	c.status.RUnlock()

	return stopped
}

func (c *Container) StatusString() string {
	var status string

	c.status.RLock()
	switch c.status.State {
	case S_CONTAINER_NONE, S_CONTAINER_CREATING:
		status = "pending"
	case S_CONTAINER_RUNNING, S_CONTAINER_STOPPING:
		status = "running"
	case S_CONTAINER_CREATED:
		status = "pending"
		if !c.status.FinishedAt.Equal(epocZero) {
			if c.status.ExitCode == 0 {
				status = "succeeded"
			} else {
				status = "failed"
			}
		}
	default:
	}
	c.status.RUnlock()

	return strings.Join([]string{c.Id(), c.SpecName(), c.p.Name, status}, ":")
}

// Container resources
func (c *Container) init(allowCreate bool) error {
	var (
		cjson  *dockertypes.ContainerJSON
		err    error
		loaded bool
	)

	if c.spec.Name == "" {
		err = fmt.Errorf("no container name provided: %#v", c.spec)
		c.Log(ERROR, err)
		return err
	}

	if c.spec.Id != "" {
		cjson = c.loadJson()
		// if label tagged this is a new container, should set `loaded` false
		loaded = true
	}

	if cjson == nil && !allowCreate {
		err = fmt.Errorf("could not load container")
		c.Log(ERROR, err)
		return err
	}

	if cjson == nil {
		cjson, err = c.createByEngine()
		if err != nil {
			c.Log(ERROR, err)
			return err
		}
	}

	c.status.CreatedAt, _ = time.Parse(time.RFC3339Nano, cjson.Created)

	desc, err := c.describeContainer(cjson)
	if err != nil {
		c.Log(ERROR, err)
		return err
	}
	desc.Volumes = c.parseVolumes(cjson)
	desc.Initialize = !loaded

	c.descript = desc

	if !loaded {
		if err = c.createVolumes(); err != nil {
			c.Log(ERROR, err)
			return err
		}

		// configEtcHosts should be called later than parse volumes, guarantee the file is not described in container
		c.configEtcHosts()

		c.configDNS()
		c.injectFiles()
	}

	return nil
}

func (c *Container) loadJson() *dockertypes.ContainerJSON {
	c.Log(TRACE, "Loading container")
	if r, err := c.p.factory.engine.ContainerInspect(c.spec.Id, false, version.Version("1.21")); err == nil {
		rsp, ok := r.(*dockertypes.ContainerJSON)
		if !ok {
			c.Log(ERROR, "fail to got loaded container info: %v", r)
			return nil
		}

		n := strings.TrimLeft(rsp.Name, "/")
		if c.spec.Name != rsp.Name {
			c.Log(ERROR, "name mismatch of loaded container, loaded is %s", n)
			c.spec.Id = ""
			return nil
		}
		c.Log(DEBUG, "Found exist container")

		return rsp
	} else {
		c.Log(ERROR, "fail to load container: %v", err)
		return nil
	}
}

func (c *Container) createByEngine() (*dockertypes.ContainerJSON, error) {
	var (
		ok  bool
		err error
		ccs dockertypes.ContainerCreateResponse
		rsp *dockertypes.ContainerJSON
		r   interface{}
	)

	config := &container.Config{
		Image:           c.spec.Image,
		Cmd:             strslice.New(c.spec.Command...),
		NetworkDisabled: true,
	}

	if len(c.spec.Entrypoint) != 0 {
		config.Entrypoint = strslice.New(c.spec.Entrypoint...)
	}

	if len(c.spec.Envs) != 0 {
		envs := []string{}
		for _, env := range c.spec.Envs {
			envs = append(envs, env.Env+"="+env.Value)
		}
		config.Env = envs
	}

	ccs, err = c.p.factory.engine.ContainerCreate(dockertypes.ContainerCreateConfig{
		Name:   c.spec.Name,
		Config: config,
	})

	if err != nil {
		return nil, err
	}

	c.Log(INFO, "create container %s", ccs.ID)
	if r, err = c.p.factory.engine.ContainerInspect(ccs.ID, false, version.Version("1.21")); err != nil {
		return nil, err
	}

	if rsp, ok = r.(*dockertypes.ContainerJSON); !ok {
		err = fmt.Errorf("fail to unpack container json response for %s of %s", c.spec.Name, c.p.Name)
		return nil, err
	}

	c.spec.Id = ccs.ID
	return rsp, nil
}

func (c *Container) describeContainer(cjson *dockertypes.ContainerJSON) (*runv.ContainerDescription, error) {

	c.Log(TRACE, "container info config %v, Cmd %v, Args %v", cjson.Config, cjson.Config.Cmd.Slice(), cjson.Args)

	if c.spec.Image == "" {
		c.spec.Image = cjson.Config.Image
	}
	c.Log(INFO, "describe container")

	mountId, err := GetMountIdByContainer(c.p.factory.sd.Type(), c.spec.Id)
	if err != nil {
		err = fmt.Errorf("Cannot find mountID for container %s : %s", c.spec.Id, err)
		c.Log(ERROR, "Cannot find mountID for container %s", err)
		return nil, err
	}

	cdesc := &runv.ContainerDescription{
		Id: c.spec.Id,

		Name:  cjson.Name, // will have a "/"
		Image: cjson.Image,

		Labels:        c.spec.Labels,
		Tty:           c.spec.Tty,
		RestartPolicy: c.spec.RestartPolicy,

		RootVolume: &runv.VolumeDescription{},
		MountId:    mountId,
		RootPath:   "rootfs",

		Envs:    make(map[string]string),
		Workdir: cjson.Config.WorkingDir,
		Path:    cjson.Path,
		Args:    cjson.Args,
		Rlimits: []*runv.Rlimit{},

		StopSignal: strings.ToUpper(cjson.Config.StopSignal),
	}

	for _, l := range c.spec.Ulimits {
		ltype := strings.ToLower(l.Name)
		append(cdesc.Rlimits, &runv.Rlimit{
			Type: ltype,
			Hard: l.Hard,
			Soft: l.Soft,
		})
	}

	if strings.HasPrefix(cdesc.StopSignal, "SIG") {
		cdesc.StopSignal = cdesc.StopSignal[len("SIG"):]
	}
	if cdesc.StopSignal == "" {
		cdesc.StopSignal = "TERM"
	}

	if cjson.Config.User != "" {
		cdesc.UGI = &runv.UserGroupInfo{
			User: cjson.Config.User,
		}
	}

	for _, v := range cjson.Config.Env {
		pair := strings.SplitN(v, "=", 2)
		if len(pair) == 2 {
			cdesc.Envs[pair[0]] = pair[1]
		} else if len(pair) == 1 {
			cdesc.Envs[pair[0]] = ""
		}
	}

	c.Log(TRACE, "Container Info is \n%#v", cdesc)

	return cdesc, nil
}

func (c *Container) parseVolumes(cjson *dockertypes.ContainerJSON) map[string]*runv.VolumeReference {

	var (
		existed = make(map[string]*apitypes.UserVolume)
		refs    = make(map[string][]*runv.VolumeReference)
	)
	for _, vol := range c.spec.Volumes {
		existed[vol.Path] = vol.Detail
		if r, ok := refs[vol.Volume]; !ok {
			refs[vol.Volume] = []*runv.VolumeReference{&runv.VolumeReference{
				Path:     vol.Path,
				Name:     vol.Volume,
				ReadOnly: vol.ReadOnly,
			}}
		} else {
			refs[vol.Volume] = append(r, &runv.VolumeReference{
				Path:     vol.Path,
				Name:     vol.Volume,
				ReadOnly: vol.ReadOnly,
			})
		}
	}

	if cjson == nil {
		return refs
	}

	for tgt := range cjson.Config.Volumes {
		if _, ok := existed[tgt]; ok {
			continue
		}

		n := c.spec.Id + strings.Replace(tgt, "/", "_", -1)
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

		c.spec.Volumes = append(c.spec.Volumes, r)
		refs[n] = []*runv.VolumeReference{&runv.VolumeReference{
			Path:     tgt,
			Name:     n,
			ReadOnly: false,
		}}
	}

	return refs

}

func (c *Container) configEtcHosts() {
	var (
		hostsVolumeName = "etchosts-volume"
		hostsVolumePath = ""
		hostsPath       = "/etc/hosts"
	)

	for _, v := range c.spec.Volumes {
		if v.Path == hostsPath {
			return nil
		}
	}

	for _, f := range c.spec.Files {
		if f.Path == hostsPath {
			return nil
		}
	}

	_, hostsVolumePath = HostsPath(c.p.Name)

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

	c.spec.Volumes = append(c.spec.Volumes, ref)
}

func (c *Container) createVolumes() error {
	var (
		err     error
		created = []string{}
	)

	defer func() {
		if err != nil {
			for _, v := range created {
				c.p.factory.sd.RemoveVolume(c.p.Name, v)
			}
		}
	}()

	for _, v := range c.spec.Volumes {
		if v.Detail == nil || v.Detail.Source != "" {
			continue
		}
		c.Log(INFO, "create volume %s", v.Volume)

		err = c.p.factory.sd.CreateVolume(c.p.Name, v.Detail)
		if err != nil {
			c.Log(ERROR, "failed to create volume %s: %v", v.Volume, err)
			return err
		}
		created = append(created, v.Volume)
	}
	return nil
}

/***
  configDNS() Set the resolv.conf of host to each container, except the following cases:

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
func (c *Container) configDNS() {
	var (
		resolvconf = "/etc/resolv.conf"
		fileId     = c.p.Name + "-resolvconf"
	)

	if len(c.p.globalSpec.Dns) > 0 {
		c.Log(DEBUG, "Already has DNS config, bypass DNS insert")
		return
	}

	if stat, e := os.Stat(resolvconf); e != nil || !stat.Mode().IsRegular() {
		c.Log(DEBUG, "Host resolv.conf does not exist or not a regular file, do not insert DNS conf")
		return
	}

	for _, vol := range c.spec.Volumes {
		if vol.Path == resolvconf {
			c.Log(DEBUG, "Already has resolv.conf configured, bypass DNS insert")
			return
		}
	}

	for _, ref := range c.spec.Files {
		if ref.Path == resolvconf || (ref.Path+ref.Filename) == resolvconf ||
			(ref.Detail != nil && ref.Detail.Uri == "file:///etc/resolv.conf") {
			c.Log(DEBUG, "Already has resolv.conf configured, bypass DNS insert")
			return
		}
	}

	c.spec.Files = append(c.spec.Files, &apitypes.UserFileReference{
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

func (c *Container) injectFiles() error {
	if len(c.spec.Files) == 0 {
		return nil
	}

	var (
		sharedDir = filepath.Join(hypervisor.BaseDir, c.p.Name, hypervisor.ShareDirTag)
	)

	for _, f := range c.spec.Files {
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

		err := c.p.factory.sd.InjectFile(src, c.spec.Id, targetPath, sharedDir,
			utils.PermInt(f.Perm), utils.UidInt(f.User), utils.UidInt(f.Group))
		if err != nil {
			c.Log(ERROR, "got error when inject files: %v", err)
			return err
		}
	}

	return nil
}

func (c *Container) volumes() []*apitypes.UserVolume {
	var (
		result  = []*apitypes.UserVolume{}
		existed = make(map[string]bool)
	)

	for _, v := range c.spec.Volumes {
		if existed[v.Volume] || v.Detail == nil {
			continue
		}
		result = append(result, v.Detail)
		existed[v.Volume] = true
	}

	return result
}

func (c *Container) addToSandbox() error {
	var (
		volmap = make(map[string]bool)
		wg     = &utils.WaitGroupWithFail{}
	)
	for _, v := range c.spec.Volumes() {
		if volmap[v.Volume] {
			continue
		}
		if vol, ok := c.p.volumes[v.Volume]; ok {
			volmap[v.Volume] = true
			if err := vol.subscribeInsert(wg); err != nil {
				c.Log(ERROR, "container depends on an impossible volume: %v", err)
				return err
			}
		}
	}

	root, err := c.p.factory.sd.PrepareContainer(c.descript.MountId, c.p.sandboxShareDir())
	if err != nil {
		c.Log(ERROR, "failed to prepare rootfs: %v", err)
		return err
	}
	c.descript.RootVolume = root

	if len(volmap) > 0 {
		err := wg.Wait()
		if err != nil {
			c.Log(ERROR, "ailed to add volume: %v", err)
			return err
		}
	}

	r := c.p.sandbox.AddContainer(c.descript)
	if !r.IsSuccess() {
		err := fmt.Errorf("failed to add container to sandbox: %s", r.Message())
		c.Log(ERROR, err)
		return err
	}

	return nil
}

func (c *Container) initLogger() {
	if c.logger.Driver != nil {
		return
	}

	if c.p.factory.logCreator == nil {
		return
	}

	ctx := logger.Context{
		Config:              c.p.factory.logCfg.Config,
		ContainerID:         c.Id(),
		ContainerName:       c.RuntimeName(),
		ContainerImageName:  c.descript.Image,
		ContainerCreated:    c.status.CreatedAt,
		ContainerEntrypoint: c.descript.Path,
		ContainerArgs:       c.descript.Args,
		ContainerImageID:    c.descript.Image,
	}

	if c.p.factory.logCfg.Type == jsonfilelog.Name {
		prefix := c.p.factory.logCfg.PathPrefix
		if c.p.factory.logCfg.PodIdInPath {
			prefix = filepath.Join(prefix, c.p.Name)
		}
		if err := os.MkdirAll(prefix, os.FileMode(0755)); err != nil {
			c.Log(ERROR, "cannot create container log dir %s: %v", prefix, err)
			return
		}

		ctx.LogPath = filepath.Join(prefix, fmt.Sprintf("%s-json.log", c.Id()))
		c.Log(DEBUG, "configure container log to ", ctx.LogPath)
	}

	driver, err := c.p.factory.logCreator(ctx)
	if err != nil {
		return
	}
	c.logger.Driver = driver
	c.Log(DEBUG, "configured logger for %s/%s (%s)", c.p.Name, c.Id(), c.RuntimeName())

	return
}

func (c *Container) startLogging() {
	var err error

	c.initLogger()

	if c.logger.Driver == nil {
		return
	}

	var stdout, stderr io.Reader

	if stdout, stderr, err = c.p.sandbox.GetLogOutput(c.Id(), nil); err != nil {
		return
	}
	c.logger.Copier = logger.NewCopier(c.Id(), map[string]io.Reader{"stdout": stdout, "stderr": stderr}, c.logger.Driver)
	c.logger.Copier.Run()

	if jl, ok := c.logger.Driver.(*jsonfilelog.JSONFileLogger); ok {
		c.logger.LogPath = jl.LogPath()
	}

	return
}

func (c *Container) getLogger() logger.Logger {
	if c.logger.Driver == nil && c.p.factory.logCreator != nil {
		c.initLogger()
	}
	return c.logger.Driver
}

func (c *Container) waitFinish(timeout int) {
	var firstStop bool

	result := c.p.sandbox.WaitProcess(true, []string{c.Id()}, timeout)
	if result == nil {
		c.Log(INFO, "wait container failed")
		firstStop = c.status.UnexpectedStopped()
	} else {
		r, ok := <-result
		if !ok {
			if timeout < 0 {
				c.Log(INFO, "container unexpected failed, chan broken")
				firstStop = c.status.UnexpectedStopped()
			}
		} else {
			c.Log(INFO, "container exited with code %v", r.Code)
			firstStop = c.status.Stopped(r.FinishedAt, r.Code)
		}
	}

	if firstStop {
		c.Log(INFO, "clean up container")
		if c.logger.Driver != nil {
			c.logger.Driver.Close()
		}
	}
}

func (c *Container) terminate() (err error) {
	if c.descript == nil {
		return
	}

	defer func() {
		if pe := recover(); pe != nil {
			err = fmt.Errorf("panic during killing container: %v", pe)
			c.Log(ERROR, err)
		}
	}()

	sig := utils.StringToSignal(c.descript.StopSignal)
	c.Log(DEBUG, "stopping: killing container with %d", sig)
	err = c.p.sandbox.KillContainer(c.Id(), sig)
	if err != nil {
		c.Log(ERROR, "failed to kill container: %v", err)
	}

	return err
}

func (c *Container) removeFromSandbox() error {
	r := c.p.sandbox.RemoveContainer(c.Id())
	if !r.IsSuccess() {
		err := fmt.Errorf("failed to remove container: %s", r.Message())
		c.Log(ERROR, err)
		return err
	}
	c.Log(DEBUG, "removed container from sandbox")
	return nil
}

func (c *Container) umountRootVol() error {
	err := c.p.factory.sd.CleanupContainer(c.Id(), c.p.sandboxShareDir())
	if err != nil {
		c.Log(ERROR, "failed to umount root volume: %v", err)
		return err
	}
	c.Log(DEBUG, "umounted root volume")
	return nil
}

func (c *Container) rename(name string) error {
	var err error
	old := c.SpecName()
	if !utils.DockerRestrictedNamePattern.MatchString(name) {
		err = fmt.Errorf("Invalid container name (%s), only %s are allowed", name, utils.DockerRestrictedNameChars)
		c.Log(ERROR, err)
		return err
	}
	if c.Id() != "" || c.descript != nil {
		err = c.p.factory.engine.ContainerRename(old, name)
		if err != nil {
			return err
		}
	}
	if name[0] == '/' {
		c.spec.Name = name
		if c.descript != nil {
			c.descript.Name = "/" + name
		}
	} else {
		c.spec.Name = name[1:]
		if c.descript != nil {
			c.descript.Name = name
		}
	}
	c.spec.Name = name
	return err
}

func (c *Container) removeFromEngine() error {
	return c.p.factory.engine.ContainerRm(c.Id(), &dockertypes.ContainerRmConfig{})
}

// container status transition
func (cs *ContainerStatus) Create() error {
	cs.Lock()
	defer cs.Unlock()

	if cs.State != S_CONTAINER_NONE {
		err := fmt.Errorf("only NONE container could be create, current: %d", cs.State)
		return err
	}

	cs.State = S_CONTAINER_CREATING

	return nil
}

func (cs *ContainerStatus) Created(t time.Time) error {
	cs.Lock()
	defer cs.Unlock()
	if cs.State != S_CONTAINER_CREATING {
		return fmt.Errorf("only CREATING container could be set to creatd, current: %d", cs.State)
	}

	cs.State = S_CONTAINER_CREATED
	cs.CreatedAt = t

	return nil
}

func (cs *ContainerStatus) Start() error {
	cs.Lock()
	defer cs.Unlock()

	if cs.State != S_CONTAINER_CREATED {
		return fmt.Errorf("only CREATING container could be set to creatd, current: %d", cs.State)
	}

	cs.State = S_CONTAINER_RUNNING

	return nil
}

func (cs *ContainerStatus) Running(t time.Time) error {
	cs.Lock()
	defer cs.Unlock()

	if cs.State != S_CONTAINER_RUNNING {
		return fmt.Errorf("only RUNNING container could set started time, current: %d", cs.State)
	}
	cs.StartedAt =  t
	return nil
}

func (cs *ContainerStatus) Stop() error {
	cs.Lock()
	defer cs.Unlock()

	if cs.State != S_CONTAINER_RUNNING {
		return fmt.Errorf("only RUNNING container could be stopped, current: %d", cs.State)
	}
	cs.State = S_CONTAINER_STOPPING
	return nil
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

