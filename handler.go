package proxy

// Copyright 2017 Michal Witkowski. All Rights Reserved.
// See LICENSE for licensing terms.

import (
	"io"
	"strings"
	"time"

	"github.com/phpstudyer/protoreflect/desc"

	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"git.xuekaole.com/smc/engine/mq"
	"github.com/phpstudyer/protoreflect/grpcreflect"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grv "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
)

var (
	clientStreamDescForProxying = &grpc.StreamDesc{
		ServerStreams: true,
		ClientStreams: true,
	}
)

// RegisterService sets up a proxy handler for a particular(详细的) gRPC service and method.
// The behaviour is the same as if you were registering a handler method, e.g. from a codegenerated pb.go file.
//
// This can *only* be used if the `server` also uses grpcproxy.CodecForServer() ServerOption.
func RegisterService(server *grpc.Server, director StreamDirector, serviceName string, methodNames ...string) {
	streamer := &handler{director: director}
	fakeDesc := &grpc.ServiceDesc{
		ServiceName: serviceName,
		HandlerType: (*interface{})(nil),
	}
	for _, m := range methodNames {
		streamDesc := grpc.StreamDesc{
			StreamName:    m,
			Handler:       streamer.handler,
			ServerStreams: true,
			ClientStreams: true,
		}
		fakeDesc.Streams = append(fakeDesc.Streams, streamDesc)
	}
	server.RegisterService(fakeDesc, streamer)
}

// TransparentHandler returns a handler that attempts to proxy all requests that are not registered in the server.
// The indented use here is as a transparent proxy, where the server doesn't know about the services implemented by the
// backends. It should be used as a `grpc.UnknownServiceHandler`.
//
// This can *only* be used if the `server` also uses grpcproxy.CodecForServer() ServerOption.
func TransparentHandler(mq mq.MQ, director StreamDirector) grpc.StreamHandler {
	streamer := &handler{mq, director}
	return streamer.handler
}

type handler struct {
	mq       mq.MQ
	director StreamDirector
}

// handler is where the real magic of proxying happens.
// It is invoked like any gRPC server stream and uses the gRPC server framing to get and receive bytes from the wire,
// forwarding it to a ClientStream established against the relevant ClientConn.
func (s *handler) handler(srv interface{}, serverStream grpc.ServerStream) error {
	var (
		body      *mq.Monitor
		methodDes *desc.MethodDescriptor
	)
	start := time.Now()
	defer func() {
		end := time.Now()
		if body != nil {
			body.IsStream, body.EndTime, body.Duration = false, end, end.Sub(start).Milliseconds()
			if methodDes != nil && (methodDes.IsClientStreaming() || methodDes.IsServerStreaming()) {
				return
			}
			go s.mq.Send(body)
		}
	}()

	// little bit of gRPC internals never hurt anyone
	fullMethodName, ok := grpc.MethodFromServerStream(serverStream)
	if !ok {
		return grpc.Errorf(codes.Internal, "lowLevelServerStream not exists in context")
	}
	// We require that the director's returned context inherits from the serverStream.Context().
	outgoingCtx, backendConn, err := s.director(serverStream.Context(), fullMethodName)
	if err != nil {
		return err
	}
	md, ok := metadata.FromIncomingContext(outgoingCtx)
	if !ok {
		return status.Error(codes.PermissionDenied, "Parse request context failed")
	}
	requestID := md.Get("requestid")
	if len(requestID) == 0 || requestID[0] == "" {
		return status.Error(codes.PermissionDenied, "Must params not passed")
	}
	method := strings.Split(fullMethodName, "/")
	body = &mq.Monitor{
		Service:   method[1],
		Method:    method[2],
		ServiceIP: backendConn.Target(),
		RequestID: requestID[0],
		Created:   start,
	}
	// protoreflect client
	client := grpcreflect.NewClient(outgoingCtx, grv.NewServerReflectionClient(backendConn))

	serviceDes, err := client.ResolveService(method[1])
	if err != nil {
		return err
	}
	methodDes = serviceDes.FindMethodByName(method[2])
	if methodDes == nil {
		return grpc.Errorf(codes.NotFound, "method not found")

	}

	if methodDes.IsClientStreaming() || methodDes.IsServerStreaming() {
		//流,只记录请求次数
		end := time.Now()
		body.IsStream, body.EndTime, body.Duration = true, end, end.Sub(start).Milliseconds()
		go s.mq.Send(body)
	}

	clientCtx, clientCancel := context.WithCancel(outgoingCtx)
	// TODO(mwitkow): Add a `forwarded` header to metadata, https://en.wikipedia.org/wiki/X-Forwarded-For.
	clientStream, err := grpc.NewClientStream(clientCtx, clientStreamDescForProxying, backendConn, fullMethodName)
	if err != nil {
		body.ErrMsg = err.Error()
		return err
	}
	// Explicitly *do not close* s2cErrChan and c2sErrChan, otherwise the select below will not terminate.
	// Channels do not have to be closed, it is just a control flow mechanism, see
	// https://groups.google.com/forum/#!msg/golang-nuts/pZwdYRGxCIk/qpbHxRRPJdUJ
	s2cErrChan := s.forwardServerToClient(serverStream, clientStream)
	c2sErrChan := s.forwardClientToServer(clientStream, serverStream)
	// We don't know which side is going to stop sending first, so we need a select between the two.
	for i := 0; i < 2; i++ {
		select {
		case s2cErr := <-s2cErrChan:
			if s2cErr == io.EOF {
				// this is the happy case where the sender has encountered io.EOF, and won't be sending anymore./
				// the clientStream>serverStream may continue pumping though.
				clientStream.CloseSend()
				break
			} else {
				// however, we may have gotten a receive error (stream disconnected, a read error etc) in which case we need
				// to cancel the clientStream to the backend, let all of its goroutines be freed up by the CancelFunc and
				// exit with an error to the stack
				clientCancel()

				if err, ok := status.FromError(s2cErr); ok && err.Code() == codes.Internal {
					body.ErrMsg = err.Message()
				}

				return grpc.Errorf(codes.Internal, "failed proxying s2c: %v", s2cErr)
			}
		case c2sErr := <-c2sErrChan:
			// This happens when the clientStream has nothing else to offer (io.EOF), returned a gRPC error. In those two
			// cases we may have received Trailers as part of the call. In case of other errors (stream closed) the trailers
			// will be nil.
			serverStream.SetTrailer(clientStream.Trailer())
			// c2sErr will contain RPC error from client code. If not io.EOF return the RPC error as server stream error.
			if c2sErr != io.EOF {
				if err, ok := status.FromError(c2sErr); ok && err.Code() == codes.Internal {
					body.ErrMsg = err.Message()
				}
				return c2sErr
			}
			return nil
		}
	}
	body.ErrMsg = "gRPC proxying should never reach this stage"
	return grpc.Errorf(codes.Internal, "gRPC proxying should never reach this stage.")
}

func (s *handler) forwardClientToServer(src grpc.ClientStream, dst grpc.ServerStream) chan error {
	ret := make(chan error, 1)
	go func() {
		f := &frame{}
		for i := 0; ; i++ {
			if err := src.RecvMsg(f); err != nil {
				ret <- err // this can be io.EOF which is happy case
				break
			}
			if i == 0 {
				// This is a bit of a hack, but client to server headers are only readable after first client msg is
				// received but must be written to server stream before the first msg is flushed.
				// This is the only place to do it nicely.
				md, err := src.Header()
				if err != nil {
					ret <- err
					break
				}
				if err := dst.SendHeader(md); err != nil {
					ret <- err
					break
				}
			}
			if err := dst.SendMsg(f); err != nil {
				ret <- err
				break
			}
		}
	}()
	return ret
}

func (s *handler) forwardServerToClient(src grpc.ServerStream, dst grpc.ClientStream) chan error {
	ret := make(chan error, 1)
	go func() {
		f := &frame{}
		for i := 0; ; i++ {
			if err := src.RecvMsg(f); err != nil {
				ret <- err // this can be io.EOF which is happy case
				break
			}
			if err := dst.SendMsg(f); err != nil {
				ret <- err
				break
			}
		}
	}()
	return ret
}
