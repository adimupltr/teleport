/*
Copyright 2017 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package proxy

import (
	"context"
	"net"
	"net/url"
	"time"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/api/client"
	apiclient "github.com/gravitational/teleport/api/client"
	apiproxy "github.com/gravitational/teleport/api/client/proxy"
	tracessh "github.com/gravitational/teleport/api/observability/tracing/ssh"
	"github.com/gravitational/teleport/lib/utils"

	"github.com/gravitational/trace"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

var log = logrus.WithFields(logrus.Fields{
	trace.Component: teleport.ComponentConnectProxy,
})

// A Dialer is a means for a client to establish a SSH connection.
type Dialer interface {
	// Dial establishes a client connection to a SSH server.
	Dial(ctx context.Context, network string, addr string, config *ssh.ClientConfig) (*tracessh.Client, error)

	// DialTimeout acts like Dial but takes a timeout.
	DialTimeout(ctx context.Context, network, address string, timeout time.Duration) (net.Conn, error)
}

type directDial struct {
	// insecure is whether to skip certificate validation.
	insecure bool
	// tlsRoutingEnabled indicates that proxy is running in TLSRouting mode.
	tlsRoutingEnabled bool
	// tlsRoutingDialerConfig is the config for TLS routing dialer when
	// TLSRouting is enabled.
	tlsRoutingDialerConfig *client.TLSRoutingDialerConfig
}

// getTLSRoutingDialerConfig creates the ALPN dialer config with provided specified
// address and timeout.
func (d directDial) getTLSRoutingDialerConfig(serverName *utils.NetAddr, timeout time.Duration) (client.TLSRoutingDialerConfig, error) {
	if d.tlsRoutingDialerConfig == nil || d.tlsRoutingDialerConfig.TLSConfig == nil {
		return client.TLSRoutingDialerConfig{}, trace.BadParameter("missing TLS config")
	}

	tlsConfig := d.tlsRoutingDialerConfig.TLSConfig.Clone()
	tlsConfig.ServerName = serverName.Host()
	tlsConfig.InsecureSkipVerify = d.insecure

	// Overwrite TLSConfig and DialTimeout.
	tlsRoutingDialerConfig := *d.tlsRoutingDialerConfig
	tlsRoutingDialerConfig.DialTimeout = timeout
	tlsRoutingDialerConfig.TLSConfig = tlsConfig

	return tlsRoutingDialerConfig, nil
}

// Dial calls ssh.Dial directly.
func (d directDial) Dial(ctx context.Context, network string, addr string, config *ssh.ClientConfig) (*tracessh.Client, error) {
	conn, err := d.DialTimeout(ctx, network, addr, config.Timeout)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return tracessh.NewClientConnWithDeadline(ctx, conn, addr, config)
}

// DialTimeout acts like Dial but takes a timeout.
func (d directDial) DialTimeout(ctx context.Context, network, address string, timeout time.Duration) (net.Conn, error) {
	if d.tlsRoutingEnabled {
		serverName, err := utils.ParseAddr(address)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		dialerConfig, err := d.getTLSRoutingDialerConfig(serverName, timeout)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		tlsDialer := client.NewTLSRoutingDialer(dialerConfig)
		tlsConn, err := tlsDialer.DialContext(ctx, "tcp", address)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return tlsConn, nil
	}

	dialer := &net.Dialer{
		Timeout: timeout,
	}

	conn, err := dialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return conn, nil
}

type proxyDial struct {
	// proxyHost is the HTTPS proxy address.
	proxyURL *url.URL
	// insecure is whether to skip certificate validation.
	insecure bool
	// tlsRoutingEnabled indicates that proxy is running in TLSRouting mode.
	tlsRoutingEnabled bool
	// tlsRoutingDialerConfig is the config for TLS routing dialer when
	// TLSRouting is enabled.
	tlsRoutingDialerConfig *client.TLSRoutingDialerConfig
}

// getTLSRoutingDialerConfig creates the ALPN dialer config with provided specified
// address and timeout.
func (d proxyDial) getTLSRoutingDialerConfig(serverName *utils.NetAddr, timeout time.Duration) (client.TLSRoutingDialerConfig, error) {
	if d.tlsRoutingDialerConfig == nil || d.tlsRoutingDialerConfig.TLSConfig == nil {
		return client.TLSRoutingDialerConfig{}, trace.BadParameter("missing TLS config")
	}

	tlsConfig := d.tlsRoutingDialerConfig.TLSConfig.Clone()
	tlsConfig.ServerName = serverName.Host()
	tlsConfig.InsecureSkipVerify = d.insecure

	// Overwrite TLSConfig and DialTimeout.
	tlsRoutingDialerConfig := *d.tlsRoutingDialerConfig
	tlsRoutingDialerConfig.DialTimeout = timeout
	tlsRoutingDialerConfig.TLSConfig = tlsConfig

	return tlsRoutingDialerConfig, nil
}

// DialTimeout acts like Dial but takes a timeout.
func (d proxyDial) DialTimeout(ctx context.Context, network, address string, timeout time.Duration) (net.Conn, error) {
	// Build a proxy connection first.
	if timeout > 0 {
		timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		ctx = timeoutCtx
	}
	conn, err := apiclient.DialProxy(ctx, d.proxyURL, address)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if d.tlsRoutingEnabled {
		serverName, err := utils.ParseAddr(address)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		dialerConfig, err := d.getTLSRoutingDialerConfig(serverName, timeout)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		tlsDialer := client.NewTLSRoutingDialer(dialerConfig)
		tlsConn, err := tlsDialer.DialContext(ctx, "tcp", address)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		conn = tlsConn
	}
	return conn, nil
}

// Dial first connects to a proxy, then uses the connection to establish a new
// SSH connection.
func (d proxyDial) Dial(ctx context.Context, network string, addr string, config *ssh.ClientConfig) (*tracessh.Client, error) {
	conn, err := d.DialTimeout(ctx, network, addr, config.Timeout)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return tracessh.NewClientConnWithDeadline(ctx, conn, addr, config)
}

type dialerOptions struct {
	// insecureSkipTLSVerify is whether to skip certificate validation.
	insecureSkipTLSVerify bool
	// tlsRoutingEnabled indicates that proxy is running in TLSRouting mode.
	tlsRoutingEnabled bool
	// tlsRoutingDialerConfig is the config for ALPN dialer when TLSRouting is enabled.
	tlsRoutingDialerConfig *client.TLSRoutingDialerConfig
}

// DialerOptionFunc allows setting options as functional arguments to DialerFromEnvironment
type DialerOptionFunc func(options *dialerOptions)

// WithTLSRoutingDialer creates a dialer that allows to Teleport running in single-port mode.
func WithTLSRoutingDialer(tlsRoutingDialerConfig *client.TLSRoutingDialerConfig) DialerOptionFunc {
	return func(options *dialerOptions) {
		options.tlsRoutingEnabled = true
		options.tlsRoutingDialerConfig = tlsRoutingDialerConfig
	}
}

// WithInsecureSkipTLSVerify skips the certs verifications.
func WithInsecureSkipTLSVerify(insecure bool) DialerOptionFunc {
	return func(options *dialerOptions) {
		options.insecureSkipTLSVerify = insecure
	}
}

// DialerFromEnvironment returns a Dial function. If the https_proxy or http_proxy
// environment variable are set, it returns a function that will dial through
// said proxy server. If neither variable is set, it will connect to the SSH
// server directly.
func DialerFromEnvironment(addr string, opts ...DialerOptionFunc) Dialer {
	// Try and get proxy addr from the environment.
	proxyURL := apiproxy.GetProxyURL(addr)

	var options dialerOptions
	for _, opt := range opts {
		opt(&options)
	}

	// If no proxy settings are in environment return regular ssh dialer,
	// otherwise return a proxy dialer.
	if proxyURL == nil {
		log.Debugf("No proxy set in environment, returning direct dialer.")
		return directDial{
			tlsRoutingEnabled:      options.tlsRoutingEnabled,
			tlsRoutingDialerConfig: options.tlsRoutingDialerConfig,
			insecure:               options.insecureSkipTLSVerify,
		}
	}
	log.Debugf("Found proxy %q in environment, returning proxy dialer.", proxyURL)
	return proxyDial{
		proxyURL:               proxyURL,
		insecure:               options.insecureSkipTLSVerify,
		tlsRoutingEnabled:      options.tlsRoutingEnabled,
		tlsRoutingDialerConfig: options.tlsRoutingDialerConfig,
	}
}

type DirectDialerOptFunc func(dial *directDial)
