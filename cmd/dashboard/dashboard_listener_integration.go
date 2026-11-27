//go:build agentcompat

package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
)

const (
	dashboardHTTPListenerFDEnv    = "NEZHA_AGENTCOMPAT_HTTP_LISTENER_FD"
	dashboardHTTPSListenerFDEnv   = "NEZHA_AGENTCOMPAT_HTTPS_LISTENER_FD"
	dashboardReceiptListenerFDEnv = "NEZHA_AGENTCOMPAT_RECEIPT_LISTENER_FD"
)

func openReceiptGateListener() (net.Listener, error) {
	if os.Getenv(dashboardReceiptListenerFDEnv) == "" {
		return nil, nil
	}
	return openDashboardListener("tcp", "127.0.0.1:0", dashboardReceiptListener)
}

var (
	errDashboardListenerKind     = errors.New("unsupported dashboard listener kind")
	errDashboardListenerTCP      = errors.New("inherited dashboard listener is not TCP")
	errDashboardListenerLoopback = errors.New("inherited dashboard listener is not loopback")
)

type dashboardListenerError struct {
	Kind                dashboardListenerKind
	EnvironmentVariable string
	Descriptor          string
	Err                 error
}

func (e *dashboardListenerError) Error() string {
	return fmt.Sprintf("adopt %s dashboard listener from %s=%q: %v", e.Kind, e.EnvironmentVariable, e.Descriptor, e.Err)
}

func (e *dashboardListenerError) Unwrap() error {
	return e.Err
}

func openDashboardListener(network, address string, kind dashboardListenerKind) (net.Listener, error) {
	environmentVariable, err := dashboardListenerEnvironmentVariable(kind)
	if err != nil {
		return nil, err
	}
	descriptor := os.Getenv(environmentVariable)
	if descriptor == "" {
		return net.Listen(network, address)
	}

	fileDescriptor, err := strconv.Atoi(descriptor)
	if err != nil || fileDescriptor < 0 {
		if err == nil {
			err = fmt.Errorf("negative file descriptor: %d", fileDescriptor)
		}
		return nil, &dashboardListenerError{
			Kind:                kind,
			EnvironmentVariable: environmentVariable,
			Descriptor:          descriptor,
			Err:                 fmt.Errorf("parse inherited file descriptor: %w", err),
		}
	}

	inheritedFile := os.NewFile(uintptr(fileDescriptor), environmentVariable)
	if inheritedFile == nil {
		return nil, &dashboardListenerError{
			Kind:                kind,
			EnvironmentVariable: environmentVariable,
			Descriptor:          descriptor,
			Err:                 errors.New("open inherited file descriptor"),
		}
	}

	listener, listenerErr := net.FileListener(inheritedFile)
	closeErr := inheritedFile.Close()
	if listenerErr != nil {
		listenerErr = fmt.Errorf("create listener from inherited file descriptor: %w", listenerErr)
		if closeErr != nil {
			listenerErr = errors.Join(listenerErr, fmt.Errorf("close inherited file descriptor wrapper: %w", closeErr))
		}
		return nil, &dashboardListenerError{
			Kind:                kind,
			EnvironmentVariable: environmentVariable,
			Descriptor:          descriptor,
			Err:                 errors.Join(errDashboardListenerTCP, listenerErr),
		}
	}
	if closeErr != nil {
		listenerCloseErr := listener.Close()
		err = fmt.Errorf("close inherited file descriptor wrapper: %w", closeErr)
		if listenerCloseErr != nil {
			err = errors.Join(err, fmt.Errorf("close adopted listener: %w", listenerCloseErr))
		}
		return nil, &dashboardListenerError{
			Kind:                kind,
			EnvironmentVariable: environmentVariable,
			Descriptor:          descriptor,
			Err:                 err,
		}
	}

	tcpListener, ok := listener.(*net.TCPListener)
	if !ok {
		if closeErr := listener.Close(); closeErr != nil {
			err = errors.Join(errDashboardListenerTCP, fmt.Errorf("close rejected listener: %w", closeErr))
		} else {
			err = errDashboardListenerTCP
		}
		return nil, &dashboardListenerError{
			Kind:                kind,
			EnvironmentVariable: environmentVariable,
			Descriptor:          descriptor,
			Err:                 err,
		}
	}

	tcpAddress, ok := tcpListener.Addr().(*net.TCPAddr)
	if !ok {
		if closeErr := tcpListener.Close(); closeErr != nil {
			err = errors.Join(errDashboardListenerTCP, fmt.Errorf("close rejected TCP listener: %w", closeErr))
		} else {
			err = errDashboardListenerTCP
		}
		return nil, &dashboardListenerError{
			Kind:                kind,
			EnvironmentVariable: environmentVariable,
			Descriptor:          descriptor,
			Err:                 err,
		}
	}
	if !tcpAddress.IP.IsLoopback() {
		if closeErr := tcpListener.Close(); closeErr != nil {
			err = errors.Join(errDashboardListenerLoopback, fmt.Errorf("close rejected listener: %w", closeErr))
		} else {
			err = errDashboardListenerLoopback
		}
		return nil, &dashboardListenerError{
			Kind:                kind,
			EnvironmentVariable: environmentVariable,
			Descriptor:          descriptor,
			Err:                 err,
		}
	}
	return tcpListener, nil
}

func dashboardListenerEnvironmentVariable(kind dashboardListenerKind) (string, error) {
	switch kind {
	case dashboardHTTPListener:
		return dashboardHTTPListenerFDEnv, nil
	case dashboardHTTPSListener:
		return dashboardHTTPSListenerFDEnv, nil
	case dashboardReceiptListener:
		return dashboardReceiptListenerFDEnv, nil
	default:
		return "", &dashboardListenerError{Kind: kind, Err: errDashboardListenerKind}
	}
}

// serveDashboardHTTPS serves on the listener opened by openDashboardHTTPS
// (inherited or bound) with the certificate in server.TLSConfig.
func serveDashboardHTTPS(server *http.Server, listener net.Listener) (err error) {
	defer func() {
		if closeErr := listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			err = errors.Join(err, fmt.Errorf("close HTTPS dashboard listener: %w", closeErr))
		}
	}()
	return server.ServeTLS(listener, "", "")
}
