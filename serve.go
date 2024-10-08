// Copyright (c) Alex Ellis 2017. All rights reserved.
// Licensed under the MIT license. See LICENSE file in the project root for full license information.

package bootstrap

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"syscall"
	"time"

	"os"
	"os/signal"

	"github.com/gorilla/mux"
	"github.com/openfaas/faas-provider/auth"
	"github.com/openfaas/faas-provider/types"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// NameExpression for a function / service
const NameExpression = "-a-zA-Z_0-9."

var r *mux.Router

// Mark this as a Golang "package"
func init() {
	r = mux.NewRouter()
}

// Router gives access to the underlying router for when new routes need to be added.
func Router() *mux.Router {
	return r
}

// Serve load your handlers into the correct OpenFaaS route spec. This function is blocking.
func Serve(handlers *types.FaaSHandlers, config *types.FaaSConfig) {

	if config.EnableBasicAuth {
		reader := auth.ReadBasicAuthFromDisk{
			SecretMountPath: config.SecretMountPath,
		}

		credentials, err := reader.Read()
		if err != nil {
			log.Fatal(err)
		}

		handlers.FunctionLister = auth.DecorateWithBasicAuth(handlers.FunctionLister, credentials)
		handlers.DeployFunction = auth.DecorateWithBasicAuth(handlers.DeployFunction, credentials)
		handlers.DeleteFunction = auth.DecorateWithBasicAuth(handlers.DeleteFunction, credentials)
		handlers.UpdateFunction = auth.DecorateWithBasicAuth(handlers.UpdateFunction, credentials)
		handlers.FunctionStatus = auth.DecorateWithBasicAuth(handlers.FunctionStatus, credentials)
		handlers.ScaleFunction = auth.DecorateWithBasicAuth(handlers.ScaleFunction, credentials)
		handlers.Info = auth.DecorateWithBasicAuth(handlers.Info, credentials)
		handlers.Secrets = auth.DecorateWithBasicAuth(handlers.Secrets, credentials)
		handlers.Logs = auth.DecorateWithBasicAuth(handlers.Logs, credentials)
		handlers.RegisterFunction = auth.DecorateWithBasicAuth(handlers.RegisterFunction, credentials)
		// NOTE by huang-jl Invoke, KillAllInstance, Metric, ListCheckpoint function do not need auth for simplicity
	}

	hm := newHttpMetrics()

	// System (auth) endpoints
	r.HandleFunc("/system/functions", hm.InstrumentHandler(handlers.FunctionLister, "")).Methods(http.MethodGet)
	r.HandleFunc("/system/functions", hm.InstrumentHandler(handlers.DeployFunction, "")).Methods(http.MethodPost)
	r.HandleFunc("/system/functions", hm.InstrumentHandler(handlers.DeleteFunction, "")).Methods(http.MethodDelete)
	r.HandleFunc("/system/functions", hm.InstrumentHandler(handlers.UpdateFunction, "")).Methods(http.MethodPut)

	r.HandleFunc("/system/function/{name:["+NameExpression+"]+}",
		hm.InstrumentHandler(handlers.FunctionStatus, "/system/function")).Methods(http.MethodGet)
	r.HandleFunc("/system/scale-function/{name:["+NameExpression+"]+}",
		hm.InstrumentHandler(handlers.ScaleFunction, "/system/scale-function")).Methods(http.MethodPost)

	r.HandleFunc("/system/info",
		hm.InstrumentHandler(handlers.Info, "")).Methods(http.MethodGet)

	r.HandleFunc("/system/secrets",
		hm.InstrumentHandler(handlers.Secrets, "")).Methods(http.MethodGet, http.MethodPut, http.MethodPost, http.MethodDelete)

	r.HandleFunc("/system/logs",
		hm.InstrumentHandler(handlers.Logs, "")).Methods(http.MethodGet)

	r.HandleFunc("/system/namespaces", hm.InstrumentHandler(handlers.ListNamespaces, "")).Methods(http.MethodGet)

	// Only register the mutate namespace handler if it is defined
	if handlers.MutateNamespace != nil {
		r.HandleFunc("/system/namespace/{name:["+NameExpression+"]*}",
			hm.InstrumentHandler(handlers.MutateNamespace, "")).Methods(http.MethodPost, http.MethodDelete, http.MethodPut, http.MethodGet)
	} else {
		r.HandleFunc("/system/namespace/{name:["+NameExpression+"]*}",
			hm.InstrumentHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "Feature not implemented in this version of OpenFaaS", http.StatusNotImplemented)
			}), "")).Methods(http.MethodGet)
	}

	proxyHandler := handlers.FunctionProxy

	// Open endpoints
	r.HandleFunc("/function/{name:["+NameExpression+"]+}", proxyHandler)
	r.HandleFunc("/function/{name:["+NameExpression+"]+}/", proxyHandler)
	r.HandleFunc("/function/{name:["+NameExpression+"]+}/{params:.*}", proxyHandler)

	if handlers.Health != nil {
		r.HandleFunc("/healthz", handlers.Health).Methods(http.MethodGet)
	}

	if handlers.RegisterFunction != nil {
		r.HandleFunc("/system/register", handlers.RegisterFunction).Methods(http.MethodPost)
	}
	if handlers.InvokeFunction != nil {
		r.HandleFunc("/invoke/{name:["+NameExpression+"]+}", handlers.InvokeFunction)
		r.HandleFunc("/invoke/{name:["+NameExpression+"]+}/", handlers.InvokeFunction)
		r.HandleFunc("/invoke/{name:["+NameExpression+"]+}/{params:.*}", handlers.InvokeFunction)
	}
	if handlers.MetricFunction != nil {
		r.HandleFunc("/system/metrics", handlers.MetricFunction).Methods(http.MethodGet, http.MethodDelete)
	}
	if handlers.ListCheckpoint != nil {
		r.HandleFunc("/system/checkpoints", handlers.ListCheckpoint).Methods(http.MethodGet)
	}
  if handlers.KillAllInstance != nil {
	  r.HandleFunc("/danger/kill", handlers.KillAllInstance).Methods(http.MethodGet, http.MethodPost, http.MethodPut)
  }

	r.HandleFunc("/metrics", promhttp.Handler().ServeHTTP)

	readTimeout := config.ReadTimeout
	writeTimeout := config.WriteTimeout

	port := 8080
	if config.TCPPort != nil {
		port = *config.TCPPort
	}

	s := &http.Server{
		Addr:           fmt.Sprintf(":%d", port),
		ReadTimeout:    readTimeout,
		WriteTimeout:   writeTimeout,
		MaxHeaderBytes: http.DefaultMaxHeaderBytes, // 1MB - can be overridden by setting Server.MaxHeaderBytes.
		Handler:        r,
	}

	go func() {
		if err := s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Shutdown the server gracefully
	if err := s.Shutdown(ctx); err != nil {
		log.Fatalf("Server shutdown failed: %v\n", err)
	}
}
