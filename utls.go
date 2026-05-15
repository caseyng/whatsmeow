// Copyright (c) 2026 caseyng
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"context"
	"net"
	"net/http"

	utls "github.com/refraction-networking/utls"
)

// NewChromeHTTPClient returns an *http.Client whose TLS ClientHello impersonates
// Chrome via uTLS, replacing the Go stdlib JA3/JA4 fingerprint.
//
// Use before connecting:
//
//	chrome := whatsmeow.NewChromeHTTPClient()
//	client.SetWebsocketHTTPClient(chrome)
//	client.SetPreLoginHTTPClient(chrome)
func NewChromeHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					host = addr
				}
				conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				uconn := utls.UClient(conn, &utls.Config{ServerName: host}, utls.HelloChrome_Auto)
				if err := uconn.HandshakeContext(ctx); err != nil {
					conn.Close()
					return nil, err
				}
				return uconn, nil
			},
		},
	}
}
