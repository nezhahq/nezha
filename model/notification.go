package model

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/nezhahq/nezha/pkg/utils"
)

const (
	_ = iota
	NotificationRequestTypeJSON
	NotificationRequestTypeForm
)

const (
	_ = iota
	NotificationRequestMethodGET
	NotificationRequestMethodPOST
)

type NotificationServerBundle struct {
	Notification *Notification
	Server       *Server
	Loc          *time.Location
}

type Notification struct {
	Common
	Name              string `json:"name"`
	URL               string `json:"url"`
	RequestMethod     uint8  `json:"request_method"`
	RequestType       uint8  `json:"request_type"`
	RequestHeader     string `json:"request_header" gorm:"type:longtext"`
	RequestBody       string `json:"request_body" gorm:"type:longtext"`
	VerifyTLS         *bool  `json:"verify_tls,omitempty"`
	FormatMetricUnits *bool  `json:"format_metric_units,omitempty"`
}

func (ns *NotificationServerBundle) reqURL(message string) string {
	n := ns.Notification
	return ns.replaceParamsInString(n.URL, message, func(msg string) string {
		return url.QueryEscape(msg)
	})
}

func (n *Notification) reqMethod() (string, error) {
	switch n.RequestMethod {
	case NotificationRequestMethodPOST:
		return http.MethodPost, nil
	case NotificationRequestMethodGET:
		return http.MethodGet, nil
	}
	return "", errors.New("不支持的请求方式")
}

func (ns *NotificationServerBundle) reqBody(message string) (string, error) {
	n := ns.Notification
	if n.RequestMethod == NotificationRequestMethodGET || message == "" {
		return "", nil
	}
	switch n.RequestType {
	case NotificationRequestTypeJSON:
		return ns.replaceParamsInString(n.RequestBody, message, func(msg string) string {
			msgBytes, _ := json.Marshal(msg)
			return string(msgBytes)[1 : len(msgBytes)-1]
		}), nil
	case NotificationRequestTypeForm:
		data, err := utils.GjsonIter(n.RequestBody)
		if err != nil {
			return "", err
		}
		params := url.Values{}
		for k, v := range data {
			params.Add(k, ns.replaceParamsInString(v, message, nil))
		}
		return params.Encode(), nil
	}
	return "", errors.New("不支持的请求类型")
}

func (n *Notification) setContentType(req *http.Request) {
	if n.RequestMethod == NotificationRequestMethodGET {
		return
	}
	if n.RequestType == NotificationRequestTypeForm {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req.Header.Set("Content-Type", "application/json")
	}
}

func (n *Notification) setRequestHeader(req *http.Request) error {
	if n.RequestHeader == "" {
		return nil
	}
	m, err := utils.GjsonIter(n.RequestHeader)
	if err != nil {
		return err
	}
	for k, v := range m {
		req.Header.Set(k, v)
	}
	return nil
}

func (ns *NotificationServerBundle) Send(message string) error {
	n := ns.Notification
	verifyTLS := n.VerifyTLS != nil && *n.VerifyTLS

	reqBody, err := ns.reqBody(message)
	if err != nil {
		return err
	}

	reqMethod, err := n.reqMethod()
	if err != nil {
		return err
	}

	reqURL := ns.reqURL(message)
	client, err := newNotificationHTTPClient(reqURL, verifyTLS)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(reqMethod, reqURL, strings.NewReader(reqBody))
	if err != nil {
		return err
	}

	n.setContentType(req)

	if err := n.setRequestHeader(req); err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return notificationResponseError(resp)
	} else {
		_, _ = io.Copy(io.Discard, resp.Body)
	}

	return nil
}

func notificationResponseError(resp *http.Response) error {
	_, _ = io.CopyN(io.Discard, resp.Body, 4096)
	return fmt.Errorf("%d@%s", resp.StatusCode, resp.Status)
}

func newNotificationHTTPClient(rawURL string, verifyTLS bool) (*http.Client, error) {
	return utils.NewRestrictedHTTPClient(rawURL, !verifyTLS)
}

// replaceParamInString 替换字符串中的占位符
func (ns *NotificationServerBundle) replaceParamsInString(str string, message string, mod func(string) string) string {
	if mod == nil {
		mod = func(s string) string { return s }
	}

	replacements := []string{
		"#NEZHA#", mod(message),
		"#DATETIME#", mod(time.Now().In(ns.Loc).String()),
	}

	if ns.Server != nil {
		runtime := ns.Server.RuntimeSnapshot()
		replacements = append(replacements,
			"#SERVER.NAME#", mod(ns.Server.Name),
			"#SERVER.ID#", mod(fmt.Sprintf("%d", ns.Server.ID)),
		)
		if runtime.State != nil && runtime.Host != nil {
			state := runtime.State
			host := runtime.Host
			replacements = append(replacements,
				// Converted metrics
				"#SERVER.CPU#", mod(ns.formatUsage(false, state.CPU)),
				"#SERVER.MEM#", mod(ns.formatUsage(true, float64(state.MemUsed)/float64(host.MemTotal))),
				"#SERVER.SWAP#", mod(ns.formatUsage(true, float64(state.SwapUsed)/float64(host.SwapTotal))),
				"#SERVER.DISK#", mod(ns.formatUsage(true, float64(state.DiskUsed)/float64(host.DiskTotal))),
				"#SERVER.SPEEDIN#", mod(fmt.Sprintf("%s/s", ns.formatSize(state.NetInSpeed))),
				"#SERVER.SPEEDOUT#", mod(fmt.Sprintf("%s/s", ns.formatSize(state.NetOutSpeed))),
				"#SERVER.TRANSFERIN#", mod(ns.formatSize(state.NetInTransfer)),
				"#SERVER.TRANSFEROUT#", mod(ns.formatSize(state.NetOutTransfer)),

				// Raw metrics
				"#SERVER.CPUUSED#", mod(fmt.Sprintf("%f", state.CPU)),
				"#SERVER.MEMUSED#", mod(fmt.Sprintf("%d", state.MemUsed)),
				"#SERVER.SWAPUSED#", mod(fmt.Sprintf("%d", state.SwapUsed)),
				"#SERVER.DISKUSED#", mod(fmt.Sprintf("%d", state.DiskUsed)),
				"#SERVER.MEMTOTAL#", mod(fmt.Sprintf("%d", host.MemTotal)),
				"#SERVER.SWAPTOTAL#", mod(fmt.Sprintf("%d", host.SwapTotal)),
				"#SERVER.DISKTOTAL#", mod(fmt.Sprintf("%d", host.DiskTotal)),
				"#SERVER.NETINSPEED#", mod(fmt.Sprintf("%d", state.NetInSpeed)),
				"#SERVER.NETOUTSPEED#", mod(fmt.Sprintf("%d", state.NetOutSpeed)),
				"#SERVER.NETINTRANSFER#", mod(fmt.Sprintf("%d", state.NetInTransfer)),
				"#SERVER.NETOUTTRANSFER#", mod(fmt.Sprintf("%d", state.NetOutTransfer)),
				"#SERVER.LOAD1#", mod(fmt.Sprintf("%f", state.Load1)),
				"#SERVER.LOAD5#", mod(fmt.Sprintf("%f", state.Load5)),
				"#SERVER.LOAD15#", mod(fmt.Sprintf("%f", state.Load15)),
				"#SERVER.TCPCONNCOUNT#", mod(fmt.Sprintf("%d", state.TcpConnCount)),
				"#SERVER.UDPCONNCOUNT#", mod(fmt.Sprintf("%d", state.UdpConnCount)),
			)
		}

		var ipv4, ipv6, validIP string
		if ns.Server.GeoIP != nil {
			ip := ns.Server.GeoIP.IP
			if ip.IPv4Addr != "" && ip.IPv6Addr != "" {
				ipv4 = ip.IPv4Addr
				ipv6 = ip.IPv6Addr
				validIP = ipv4
			} else if ip.IPv4Addr != "" {
				ipv4 = ip.IPv4Addr
				validIP = ipv4
			} else {
				ipv6 = ip.IPv6Addr
				validIP = ipv6
			}

			replacements = append(replacements,
				"#SERVER.IP#", mod(validIP),
				"#SERVER.IPV4#", mod(ipv4),
				"#SERVER.IPV6#", mod(ipv6),
			)
		}
	}

	replacer := strings.NewReplacer(replacements...)
	return replacer.Replace(str)
}

func (ns *NotificationServerBundle) formatUsage(toPercentage bool, usage float64) string {
	if ns.Notification.FormatMetricUnits != nil && *ns.Notification.FormatMetricUnits {
		if toPercentage {
			usage = usage * 100
		}

		return fmt.Sprintf("%.2f %%", usage)
	}
	return fmt.Sprintf("%f", usage)
}

func (ns *NotificationServerBundle) formatSize(size uint64) string {
	if ns.Notification.FormatMetricUnits != nil && *ns.Notification.FormatMetricUnits {
		return utils.Bytes(size)
	}
	return fmt.Sprintf("%d", size)
}
