// SPDX-License-Identifier: Apache-2.0

package csi

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"

	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"gitlab.com/kubesan/kubesan/internal/common/config"
	csiclient "gitlab.com/kubesan/kubesan/internal/csi/common/client"
	"gitlab.com/kubesan/kubesan/internal/csi/controller"
	"gitlab.com/kubesan/kubesan/internal/csi/identity"
	"gitlab.com/kubesan/kubesan/internal/csi/node"
)

func RunControllerPlugin() error {
	return serve(true, func(server *grpc.Server, client *csiclient.CsiK8sClient) {
		csi.RegisterIdentityServer(server, &identity.IdentityServer{})
		csi.RegisterControllerServer(server, controller.NewControllerServer(client))
	})
}

func RunNodePlugin() error {
	return serve(false, func(server *grpc.Server, client *csiclient.CsiK8sClient) {
		csi.RegisterIdentityServer(server, &identity.IdentityServer{})
		csi.RegisterNodeServer(server, node.NewNodeServer(client))
	})
}

func serve(cluster bool, register func(*grpc.Server, *csiclient.CsiK8sClient)) error {
	// Set up structured logging

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	log.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// create Kubernetes client

	client, done, err := csiclient.NewCsiK8sClient(cluster)
	if err != nil {
		return err
	}

	// remove any leftover socket file

	err = os.Remove(config.CsiSocketPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	// create gRPC server

	listener, err := net.Listen("unix", config.CsiSocketPath)
	if err != nil {
		return fmt.Errorf("failed to listen: %v", err)
	}

	loggingInterceptor := func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		log := log.FromContext(ctx).WithValues("method", info.FullMethod)
		log.Info("gRPC entry", "request", req)
		resp, err := handler(ctx, req)
		if err == nil {
			log.Info("gRPC success", "response", resp)
		} else {
			log.Error(err, "gRPC failure")
		}
		return resp, err
	}

	server := grpc.NewServer(grpc.ChainUnaryInterceptor(loggingInterceptor))

	register(server, client)

	// Handle SIGTERM gracefully.

	log := log.FromContext(context.Background())
	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	log.Info("registered for shutdown signals")
	go func() {
		<-c
		log.Info("shutdown signal received, initiating graceful shutdown")
		timer := time.AfterFunc(5*time.Second, func() {
			log.Info("graceful shutdown taking too long, forcing stop")
			server.Stop()
		})
		defer timer.Stop()
		go func() {
			client.Cancel()
		}()
		server.GracefulStop()
		<-done
		log.Info("server stopped gracefully")
	}()

	// run gRPC server

	return server.Serve(listener)
}
