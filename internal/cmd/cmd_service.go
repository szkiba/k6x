// SPDX-FileCopyrightText: 2023 Iván SZKIBA
//
// SPDX-License-Identifier: AGPL-3.0-only

package cmd

import (
	"context"
	"math"
	"net"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/szkiba/k6x/internal/builder"
	"github.com/szkiba/k6x/internal/resolver"
	"github.com/szkiba/k6x/internal/service"
	"golang.org/x/net/netutil"
)

const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 5 * time.Second
	writeTimeout      = 100 * time.Second
)

func serviceCommand(
	ctx context.Context,
	res resolver.Resolver,
	opts *options,
	out *os.File, //nolint:forbidigo
) error {
	if opts.help {
		return usage(out, serviceUsage, opts)
	}

	opts.spinner.Disable()

	b, err := builder.New(ctx, opts.engines...)
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:              opts.addr,
		Handler:           recovery(service.New(res, b)),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
	}

	l, err := net.Listen("tcp", opts.addr)
	if err != nil {
		return err
	}

	defer l.Close() //nolint:errcheck

	limit := int(math.Max(2, float64(runtime.NumCPU())/4.0))

	return server.Serve(netutil.LimitListener(l, limit))
}

func recovery(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
		defer func() {
			rec := recover()
			if rec != nil {
				res.WriteHeader(http.StatusInternalServerError)

				logrus.WithField("panic", rec).Error(string(debug.Stack()))
			}
		}()

		handler.ServeHTTP(res, req)
	})
}

const serviceUsage = `Start the builder service.

Usage:
  {{.appname}} service [flags]

Flags:
  --addr address     listen address (default: 127.0.0.1:8787)
  --filter expr      jmespath syntax extension registry filter (default: [*])
  --builder list     comma separated list of builders (default: {{.builders}})

  -h, --help      display this help
`
