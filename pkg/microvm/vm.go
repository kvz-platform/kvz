//go:build darwin

// Package microvm is kvz's own Linux microVM monitor on Virtualization.framework.
// Unlike Apple's container daemon it boots VMs concurrently. Each VM gets an
// IP chosen by the host (passed on the kernel cmdline), a virtiofs share with
// its pod spec and logs, read-only virtiofs shares of image rootfs trees, and
// a vsock event channel to kvz-init.
package microvm

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/Code-Hex/vz/v3"
)

type Share struct {
	Tag      string
	Path     string
	ReadOnly bool
}

type Config struct {
	Kernel     string
	Initrd     string
	IP, GW     net.IP
	Mask       int
	CPUs       uint
	MemoryMB   uint64
	Shares     []Share
	ConsoleLog string // "" = no console
	ExtraArgs  string
}

type Event struct {
	Type      string  `json:"type"`
	Container string  `json:"container,omitempty"`
	Pid       int     `json:"pid,omitempty"`
	Code      int     `json:"code,omitempty"`
	Msg       string  `json:"msg,omitempty"`
	BootMs    float64 `json:"boot_ms,omitempty"`
	ReadyMs   float64 `json:"ready_ms,omitempty"`
}

type VM struct {
	vm     *vz.VirtualMachine
	Events chan Event
	conn   net.Conn
	mu     sync.Mutex
	done   chan struct{}
	MAC    string
	T      Timings
}

type Timings struct {
	Configure time.Duration // config build + validate
	Create    time.Duration // NewVirtualMachine
	Start     time.Duration // vm.Start returned
	Hello     time.Duration // agent connected (rootfs mounted)
}

func (c Config) cmdline() string {
	mask := net.CIDRMask(c.Mask, 32)
	m := net.IP(mask).String()
	return fmt.Sprintf("console=hvc0 quiet loglevel=1 rdinit=/init panic=-1 "+
		"ip=%s::%s:%s::eth0:off:%s random.trust_cpu=on %s", c.IP, c.GW, m, c.GW, c.ExtraArgs)
}

// Boot creates and starts a VM. It returns once the VM is running; events
// (hello, started, exit) arrive on VM.Events.
func Boot(c Config) (*VM, error) {
	t0 := time.Now()
	bl, err := vz.NewLinuxBootLoader(c.Kernel, vz.WithInitrd(c.Initrd), vz.WithCommandLine(c.cmdline()))
	if err != nil {
		return nil, err
	}
	if c.CPUs == 0 {
		c.CPUs = 1
	}
	if c.MemoryMB == 0 {
		c.MemoryMB = 512
	}
	cfg, err := vz.NewVirtualMachineConfiguration(bl, c.CPUs, c.MemoryMB<<20)
	if err != nil {
		return nil, err
	}
	// console
	if c.ConsoleLog != "" {
		att, err := vz.NewFileSerialPortAttachment(c.ConsoleLog, false)
		if err == nil {
			if sp, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(att); err == nil {
				cfg.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{sp})
			}
		}
	}
	// network: NAT, host-chosen MAC
	nat, err := vz.NewNATNetworkDeviceAttachment()
	if err != nil {
		return nil, err
	}
	nic, err := vz.NewVirtioNetworkDeviceConfiguration(nat)
	if err != nil {
		return nil, err
	}
	mac, err := vz.NewRandomLocallyAdministeredMACAddress()
	if err != nil {
		return nil, err
	}
	nic.SetMACAddress(mac)
	cfg.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{nic})
	// vsock
	vs, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return nil, err
	}
	cfg.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{vs})
	// entropy
	if ent, err := vz.NewVirtioEntropyDeviceConfiguration(); err == nil {
		cfg.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{ent})
	}
	// virtiofs
	var fsdevs []vz.DirectorySharingDeviceConfiguration
	for _, s := range c.Shares {
		dev, err := vz.NewVirtioFileSystemDeviceConfiguration(s.Tag)
		if err != nil {
			return nil, err
		}
		sd, err := vz.NewSharedDirectory(s.Path, s.ReadOnly)
		if err != nil {
			return nil, fmt.Errorf("share %s: %w", s.Path, err)
		}
		share, err := vz.NewSingleDirectoryShare(sd)
		if err != nil {
			return nil, err
		}
		dev.SetDirectoryShare(share)
		fsdevs = append(fsdevs, dev)
	}
	cfg.SetDirectorySharingDevicesVirtualMachineConfiguration(fsdevs)
	if ok, err := cfg.Validate(); !ok || err != nil {
		return nil, fmt.Errorf("invalid vm config: %v", err)
	}
	v := &VM{Events: make(chan Event, 64), done: make(chan struct{}), MAC: mac.String()}
	v.T.Configure = time.Since(t0)
	vm, err := vz.NewVirtualMachine(cfg)
	if err != nil {
		return nil, err
	}
	v.vm = vm
	v.T.Create = time.Since(t0)
	socks := vm.SocketDevices()
	if len(socks) == 0 {
		return nil, fmt.Errorf("no vsock device")
	}
	ln, err := socks[0].Listen(1024)
	if err != nil {
		return nil, err
	}
	go func() {
		conn, err := ln.Accept()
		ln.Close()
		if err != nil {
			close(v.Events)
			return
		}
		v.mu.Lock()
		v.conn = conn
		v.mu.Unlock()
		sc := bufio.NewScanner(conn)
		first := true
		for sc.Scan() {
			var e Event
			if json.Unmarshal(sc.Bytes(), &e) != nil {
				continue
			}
			if first && e.Type == "hello" {
				v.T.Hello = time.Since(t0)
				first = false
			}
			v.Events <- e
		}
		close(v.Events)
	}()
	if err := vm.Start(); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	v.T.Start = time.Since(t0)
	go func() {
		for st := range vm.StateChangedNotify() {
			if st == vz.VirtualMachineStateStopped || st == vz.VirtualMachineStateError {
				close(v.done)
				return
			}
		}
	}()
	return v, nil
}

// Send writes a command to kvz-init.
func (v *VM) Send(cmd any) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.conn == nil {
		return fmt.Errorf("agent not connected")
	}
	b, _ := json.Marshal(cmd)
	_, err := v.conn.Write(append(b, '\n'))
	return err
}

func (v *VM) Done() <-chan struct{} { return v.done }

// Stop hard-stops the VM (pod VMs hold no state worth a clean shutdown; the
// guest's writable layer is tmpfs).
func (v *VM) Stop() error {
	select {
	case <-v.done:
		return nil
	default:
	}
	if v.vm.CanStop() {
		if err := v.vm.Stop(); err != nil {
			return err
		}
	}
	select {
	case <-v.done:
	case <-time.After(5 * time.Second):
	}
	return nil
}

func (v *VM) State() vz.VirtualMachineState { return v.vm.State() }

// Dial opens a vsock connection to a guest port.
func (v *VM) Dial(port uint32) (net.Conn, error) {
	socks := v.vm.SocketDevices()
	if len(socks) == 0 {
		return nil, fmt.Errorf("no vsock device")
	}
	return socks[0].Connect(port)
}
