package model

import (
	"fmt"

	pb "github.com/nezhahq/nezha/proto"
)

const (
	_ = iota

	MTReportHostState
)

type SensorTemperature struct {
	Name        string
	Temperature float64
}

type HostState struct {
	CPU            float64             `json:"cpu,omitempty"`
	MemUsed        uint64              `json:"mem_used,omitempty"`
	SwapUsed       uint64              `json:"swap_used,omitempty"`
	DiskUsed       uint64              `json:"disk_used,omitempty"`
	NetInTransfer  uint64              `json:"net_in_transfer,omitempty"`
	NetOutTransfer uint64              `json:"net_out_transfer,omitempty"`
	NetInSpeed     uint64              `json:"net_in_speed,omitempty"`
	NetOutSpeed    uint64              `json:"net_out_speed,omitempty"`
	Uptime         uint64              `json:"uptime,omitempty"`
	Load1          float64             `json:"load_1,omitempty"`
	Load5          float64             `json:"load_5,omitempty"`
	Load15         float64             `json:"load_15,omitempty"`
	TcpConnCount   uint64              `json:"tcp_conn_count,omitempty"`
	UdpConnCount   uint64              `json:"udp_conn_count,omitempty"`
	ProcessCount   uint64              `json:"process_count,omitempty"`
	Temperatures   []SensorTemperature `json:"temperatures,omitempty"`
	GPU            []float64           `json:"gpu,omitempty"`
}

func (s *HostState) PB() *pb.State {
	var ts []*pb.State_SensorTemperature
	for _, t := range s.Temperatures {
		ts = append(ts, &pb.State_SensorTemperature{
			Name:        t.Name,
			Temperature: t.Temperature,
		})
	}

	return &pb.State{
		Cpu:            s.CPU,
		MemUsed:        s.MemUsed,
		SwapUsed:       s.SwapUsed,
		DiskUsed:       s.DiskUsed,
		NetInTransfer:  s.NetInTransfer,
		NetOutTransfer: s.NetOutTransfer,
		NetInSpeed:     s.NetInSpeed,
		NetOutSpeed:    s.NetOutSpeed,
		Uptime:         s.Uptime,
		Load1:          s.Load1,
		Load5:          s.Load5,
		Load15:         s.Load15,
		TcpConnCount:   s.TcpConnCount,
		UdpConnCount:   s.UdpConnCount,
		ProcessCount:   s.ProcessCount,
		Temperatures:   ts,
		Gpu:            s.GPU,
	}
}

func PB2State(s *pb.State) HostState {
	var ts []SensorTemperature
	for _, t := range s.GetTemperatures() {
		ts = append(ts, SensorTemperature{
			Name:        t.GetName(),
			Temperature: t.GetTemperature(),
		})
	}

	return HostState{
		CPU:            s.GetCpu(),
		MemUsed:        s.GetMemUsed(),
		SwapUsed:       s.GetSwapUsed(),
		DiskUsed:       s.GetDiskUsed(),
		NetInTransfer:  s.GetNetInTransfer(),
		NetOutTransfer: s.GetNetOutTransfer(),
		NetInSpeed:     s.GetNetInSpeed(),
		NetOutSpeed:    s.GetNetOutSpeed(),
		Uptime:         s.GetUptime(),
		Load1:          s.GetLoad1(),
		Load5:          s.GetLoad5(),
		Load15:         s.GetLoad15(),
		TcpConnCount:   s.GetTcpConnCount(),
		UdpConnCount:   s.GetUdpConnCount(),
		ProcessCount:   s.GetProcessCount(),
		Temperatures:   ts,
		GPU:            s.GetGpu(),
	}
}

type Host struct {
	Platform        string   `json:"platform,omitempty"`
	PlatformVersion string   `json:"platform_version,omitempty"`
	CPU             []string `json:"cpu,omitempty"`
	MemTotal        uint64   `json:"mem_total,omitempty"`
	DiskTotal       uint64   `json:"disk_total,omitempty"`
	SwapTotal       uint64   `json:"swap_total,omitempty"`
	Arch            string   `json:"arch,omitempty"`
	Virtualization  string   `json:"virtualization,omitempty"`
	BootTime        uint64   `json:"boot_time,omitempty"`
	Version         string   `json:"version,omitempty"`
	GPU             []string `json:"gpu,omitempty"`
}

func (h *Host) PB() *pb.Host {
	return &pb.Host{
		Platform:        h.Platform,
		PlatformVersion: h.PlatformVersion,
		Cpu:             h.CPU,
		MemTotal:        h.MemTotal,
		DiskTotal:       h.DiskTotal,
		SwapTotal:       h.SwapTotal,
		Arch:            h.Arch,
		Virtualization:  h.Virtualization,
		BootTime:        h.BootTime,
		Version:         h.Version,
		Gpu:             h.GPU,
	}
}

// Filter returns a new instance of Host with some fields redacted.
func (h *Host) Filter() *Host {
	return &Host{
		Platform:       h.Platform,
		CPU:            h.CPU,
		MemTotal:       h.MemTotal,
		DiskTotal:      h.DiskTotal,
		SwapTotal:      h.SwapTotal,
		Arch:           h.Arch,
		Virtualization: h.Virtualization,
		BootTime:       h.BootTime,
		GPU:            h.GPU,
	}
}

func PB2Host(h *pb.Host) Host {
	return Host{
		Platform:        h.GetPlatform(),
		PlatformVersion: h.GetPlatformVersion(),
		CPU:             h.GetCpu(),
		MemTotal:        h.GetMemTotal(),
		DiskTotal:       h.GetDiskTotal(),
		SwapTotal:       h.GetSwapTotal(),
		Arch:            h.GetArch(),
		Virtualization:  h.GetVirtualization(),
		BootTime:        h.GetBootTime(),
		Version:         h.GetVersion(),
		GPU:             h.GetGpu(),
	}
}

type IP struct {
	IPv4Addr string `json:"ipv4_addr,omitempty"`
	IPv6Addr string `json:"ipv6_addr,omitempty"`
}

func (p *IP) Join() string {
	if p.IPv4Addr != "" && p.IPv6Addr != "" {
		return fmt.Sprintf("%s/%s", p.IPv4Addr, p.IPv6Addr)
	} else if p.IPv4Addr != "" {
		return p.IPv4Addr
	}
	return p.IPv6Addr
}

type GeoIP struct {
	IP          IP     `json:"ip,omitempty"`
	CountryCode string `json:"country_code,omitempty"`
}

func PB2GeoIP(p *pb.GeoIP) GeoIP {
	pbIP := p.GetIp()
	return GeoIP{
		IP: IP{
			IPv4Addr: pbIP.GetIpv4(),
			IPv6Addr: pbIP.GetIpv6(),
		},
	}
}
