package model

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/nezhahq/nezha/pkg/utils"
	"github.com/tidwall/gjson"
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
	base, query, hasQuery := strings.Cut(n.URL, "?")
	reqURL := ns.replaceParamsInString(base, message, url.QueryEscape)
	if !hasQuery {
		return reqURL
	}
	// Like a form field, a query parameter may hold a JSON document.
	params := strings.Split(query, "&")
	for i, param := range params {
		escape := url.QueryEscape
		_, value, _ := strings.Cut(param, "=")
		if value, err := url.QueryUnescape(value); err == nil {
			if jsonEscape := jsonTemplateEscape(value); jsonEscape != nil {
				escape = func(msg string) string { return url.QueryEscape(jsonEscape(msg)) }
			}
		}
		params[i] = ns.replaceParamsInString(param, message, escape)
	}
	return reqURL + "?" + strings.Join(params, "&")
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
		return ns.replaceParamsInString(n.RequestBody, message, jsonStringEscape), nil
	case NotificationRequestTypeForm:
		data, err := utils.GjsonIter(n.RequestBody)
		if err != nil {
			return "", err
		}
		params := url.Values{}
		for k, v := range data {
			params.Add(k, ns.replaceParamsInString(v, message, jsonTemplateEscape(v)))
		}
		return params.Encode(), nil
	}
	return "", errors.New("不支持的请求类型")
}

// jsonStringEscape escapes msg for use inside a JSON string literal.
func jsonStringEscape(msg string) string {
	msgBytes, _ := json.Marshal(msg)
	return string(msgBytes)[1 : len(msgBytes)-1]
}

// templatePlaceholder matches the placeholders of replaceParamsInString:
// #NEZHA#, #DATETIME#, #SERVER.NAME#, ...
var templatePlaceholder = regexp.MustCompile(`#[A-Z0-9.]+#`)

// jsonTemplateEscape returns the escaping that placeholder values need before
// they are expanded in value, one form field or query parameter of a template.
// Such a field may hold a JSON document, as in Slack's
// payload={"text":"#NEZHA#"}: form encoding keeps a value inside the field but
// not inside the string literal, so it needs jsonStringEscape like a JSON
// body. For a plain text field the result is nil.
func jsonTemplateEscape(value string) func(string) string {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "{") && !strings.HasPrefix(value, "[") {
		return nil
	}
	// Without a string literal there is nothing to break out of, and
	// "[#SERVER.NAME#]" is plain text.
	if !strings.Contains(value, `"`) {
		return nil
	}
	// A placeholder may also stand for a number: {"swap":#SERVER.SWAPUSED#}.
	if !gjson.Valid(templatePlaceholder.ReplaceAllString(value, "0")) {
		return nil
	}
	return jsonStringEscape
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
	return ns.SendContext(context.Background(), message)
}

// SendContext is Send with a context that aborts the delivery, including a
// request that is already waiting for the webhook, as soon as it is canceled.
func (ns *NotificationServerBundle) SendContext(ctx context.Context, message string) error {
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
	client, err := newNotificationHTTPClient(ctx, reqURL, verifyTLS)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, reqMethod, reqURL, strings.NewReader(reqBody))
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

func newNotificationHTTPClient(ctx context.Context, rawURL string, verifyTLS bool) (*http.Client, error) {
	return utils.NewRestrictedHTTPClientContext(ctx, rawURL, !verifyTLS)
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
				"#SERVER.MEM#", mod(ns.formatUsage(true, usageRatio(state.MemUsed, host.MemTotal))),
				"#SERVER.SWAP#", mod(ns.formatUsage(true, usageRatio(state.SwapUsed, host.SwapTotal))),
				"#SERVER.DISK#", mod(ns.formatUsage(true, usageRatio(state.DiskUsed, host.DiskTotal))),
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

// usageRatio returns used/total, or 0 when the agent reports no total (e.g. no
// swap, or no host report yet), so templates never render NaN or Inf.
func usageRatio(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) / float64(total)
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
