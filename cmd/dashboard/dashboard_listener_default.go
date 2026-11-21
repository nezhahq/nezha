//go:build !agentcompat

package main

import (
	"net"
	"net/http"
)

func openReceiptGateListener() (net.Listener, error) { return nil, nil }

func openDashboardListener(network, address string, _ dashboardListenerKind) (net.Listener, error) {
	return net.Listen(network, address)
}

// serveDashboardHTTPS serves on the listener opened by openDashboardHTTPS
// with the certificate in server.TLSConfig.
func serveDashboardHTTPS(server *http.Server, listener net.Listener) error {
	return server.ServeTLS(listener, "", "")
}
