package model

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

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
	Name          string `json:"name"`
	URL           string `json:"url"`
	RequestMethod uint8  `json:"request_method"`
	RequestType   uint8  `json:"request_type"`
	RequestHeader string `json:"request_header" gorm:"type:longtext"`
	RequestBody   string `json:"request_body" gorm:"type:longtext"`
	VerifyTLS     *bool  `json:"verify_tls,omitempty"`
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
			msgBytes, _ := utils.Json.Marshal(msg)
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
	var client *http.Client
	n := ns.Notification
	if n.VerifyTLS != nil && *n.VerifyTLS {
		client = utils.HttpClient
	} else {
		client = utils.HttpClientSkipTlsVerify
	}

	reqBody, err := ns.reqBody(message)
	if err != nil {
		return err
	}

	reqMethod, err := n.reqMethod()
	if err != nil {
		return err
	}

	req, err := http.NewRequest(reqMethod, ns.reqURL(message), strings.NewReader(reqBody))
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
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%d@%s %s", resp.StatusCode, resp.Status, string(body))
	} else {
		_, _ = io.Copy(io.Discard, resp.Body)
	}

	return nil
}

// replaceParamInString 替换字符串中的占位符
func (ns *NotificationServerBundle) replaceParamsInString(str string, message string, mod func(string) string) string {
	if mod == nil {
		mod = func(s string) string {
			return s
		}
	}

	str = strings.ReplaceAll(str, "#NEZHA#", mod(message))
	str = strings.ReplaceAll(str, "#DATETIME#", mod(time.Now().In(ns.Loc).String()))

	if ns.Server != nil {
		str = strings.ReplaceAll(str, "#SERVER.NAME#", mod(ns.Server.Name))
		str = strings.ReplaceAll(str, "#SERVER.ID#", mod(fmt.Sprintf("%d", ns.Server.ID)))
		str = strings.ReplaceAll(str, "#SERVER.CPU#", mod(fmt.Sprintf("%f", ns.Server.State.CPU)))
		str = strings.ReplaceAll(str, "#SERVER.MEM#", mod(fmt.Sprintf("%d", ns.Server.State.MemUsed)))
		str = strings.ReplaceAll(str, "#SERVER.SWAP#", mod(fmt.Sprintf("%d", ns.Server.State.SwapUsed)))
		str = strings.ReplaceAll(str, "#SERVER.DISK#", mod(fmt.Sprintf("%d", ns.Server.State.DiskUsed)))
		str = strings.ReplaceAll(str, "#SERVER.MEMUSED#", mod(fmt.Sprintf("%d", ns.Server.State.MemUsed)))
		str = strings.ReplaceAll(str, "#SERVER.SWAPUSED#", mod(fmt.Sprintf("%d", ns.Server.State.SwapUsed)))
		str = strings.ReplaceAll(str, "#SERVER.DISKUSED#", mod(fmt.Sprintf("%d", ns.Server.State.DiskUsed)))
		str = strings.ReplaceAll(str, "#SERVER.MEMTOTAL#", mod(fmt.Sprintf("%d", ns.Server.Host.MemTotal)))
		str = strings.ReplaceAll(str, "#SERVER.SWAPTOTAL#", mod(fmt.Sprintf("%d", ns.Server.Host.SwapTotal)))
		str = strings.ReplaceAll(str, "#SERVER.DISKTOTAL#", mod(fmt.Sprintf("%d", ns.Server.Host.DiskTotal)))
		str = strings.ReplaceAll(str, "#SERVER.NETINSPEED#", mod(fmt.Sprintf("%d", ns.Server.State.NetInSpeed)))
		str = strings.ReplaceAll(str, "#SERVER.NETOUTSPEED#", mod(fmt.Sprintf("%d", ns.Server.State.NetOutSpeed)))
		str = strings.ReplaceAll(str, "#SERVER.TRANSFERIN#", mod(fmt.Sprintf("%d", ns.Server.State.NetInTransfer)))
		str = strings.ReplaceAll(str, "#SERVER.TRANSFEROUT#", mod(fmt.Sprintf("%d", ns.Server.State.NetOutTransfer)))
		str = strings.ReplaceAll(str, "#SERVER.NETINTRANSFER#", mod(fmt.Sprintf("%d", ns.Server.State.NetInTransfer)))
		str = strings.ReplaceAll(str, "#SERVER.NETOUTTRANSFER#", mod(fmt.Sprintf("%d", ns.Server.State.NetOutTransfer)))
		str = strings.ReplaceAll(str, "#SERVER.LOAD1#", mod(fmt.Sprintf("%f", ns.Server.State.Load1)))
		str = strings.ReplaceAll(str, "#SERVER.LOAD5#", mod(fmt.Sprintf("%f", ns.Server.State.Load5)))
		str = strings.ReplaceAll(str, "#SERVER.LOAD15#", mod(fmt.Sprintf("%f", ns.Server.State.Load15)))
		str = strings.ReplaceAll(str, "#SERVER.TCPCONNCOUNT#", mod(fmt.Sprintf("%d", ns.Server.State.TcpConnCount)))
		str = strings.ReplaceAll(str, "#SERVER.UDPCONNCOUNT#", mod(fmt.Sprintf("%d", ns.Server.State.UdpConnCount)))

		var ipv4, ipv6, validIP string
		ipList := strings.Split(ns.Server.GeoIP.IP.Join(), "/")
		if len(ipList) > 1 {
			// 双栈
			ipv4 = ipList[0]
			ipv6 = ipList[1]
			validIP = ipv4
		} else if len(ipList) == 1 {
			// 仅ipv4|ipv6
			if strings.IndexByte(ipList[0], ':') != -1 {
				ipv6 = ipList[0]
				validIP = ipv6
			} else {
				ipv4 = ipList[0]
				validIP = ipv4
			}
		}

		str = strings.ReplaceAll(str, "#SERVER.IP#", mod(validIP))
		str = strings.ReplaceAll(str, "#SERVER.IPV4#", mod(ipv4))
		str = strings.ReplaceAll(str, "#SERVER.IPV6#", mod(ipv6))
	}

	return str
}
