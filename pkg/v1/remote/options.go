// Copyright 2018 Google LLC All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package remote

import (
	"context"
	"errors"
	"io"
	"net/http"
	"syscall"
	"time"

	"github.com/google/go-containerregistry/internal/retry"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/logs"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// Option is a functional option for remote operations.
type Option func(*options) error

type options struct {
	auth                           authn.Authenticator
	keychain                       authn.Keychain
	transport                      http.RoundTripper
	platform                       v1.Platform
	context                        context.Context
	jobs                           int
	userAgent                      string
	allowNondistributableArtifacts bool
	updates                        chan<- v1.Update
	pageSize                       int
	retryBackoff                   Backoff
	retryPredicate                 retry.Predicate
}

var defaultPlatform = v1.Platform{
	Architecture: "amd64",
	OS:           "linux",
}

// Backoff is an alias of retry.Backoff to expose this configuration option to consumers of this lib
type Backoff = retry.Backoff

var defaultRetryPredicate retry.Predicate = func(err error) bool {
	// Various failure modes here, as we're often reading from and writing to
	// the network.
	if retry.IsTemporary(err) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.EPIPE) {
		logs.Warn.Printf("retrying %v", err)
		return true
	}
	return false
}

// Try this three times, waiting 1s after first failure, 3s after second.
var defaultRetryBackoff = Backoff{
	Duration: 1.0 * time.Second,
	Factor:   3.0,
	Jitter:   0.1,
	Steps:    3,
}

const (
	defaultJobs = 4

	// ECR returns an error if n > 1000:
	// https://github.com/google/go-containerregistry/issues/1091
	defaultPageSize = 1000
)

func makeOptions(target authn.Resource, opts ...Option) (*options, error) {
	o := &options{
		auth:           authn.Anonymous,
		transport:      http.DefaultTransport,
		platform:       defaultPlatform,
		context:        context.Background(),
		jobs:           defaultJobs,
		pageSize:       defaultPageSize,
		retryPredicate: defaultRetryPredicate,
		retryBackoff:   defaultRetryBackoff,
	}

	for _, option := range opts {
		if err := option(o); err != nil {
			return nil, err
		}
	}

	if o.keychain != nil {
		auth, err := o.keychain.Resolve(target)
		if err != nil {
			return nil, err
		}
		o.auth = auth
	}

	if _, ok := o.transport.(*transport.Transport); !ok {
		// Wrap the transport in something that logs requests and responses.
		// It's expensive to generate the dumps, so skip it if we're writing
		// to nothing.
		if logs.Enabled(logs.Debug) {
			o.transport = transport.NewLogger(o.transport)
		}

		// Wrap the transport in something that can retry network flakes.
		o.transport = transport.NewRetry(o.transport, transport.WithRetryBackoff(o.retryBackoff), transport.WithRetryPredicate(o.retryPredicate))

		// Wrap this last to prevent transport.New from double-wrapping.
		if o.userAgent != "" {
			o.transport = transport.NewUserAgent(o.transport, o.userAgent)
		}
	}

	return o, nil
}

// WithTransport is a functional option for overriding the default transport
// for remote operations.
//
// The default transport its http.DefaultTransport.
func WithTransport(t http.RoundTripper) Option {
	return func(o *options) error {
		o.transport = t
		return nil
	}
}

// WithAuth is a functional option for overriding the default authenticator
// for remote operations.
//
// The default authenticator is authn.Anonymous.
func WithAuth(auth authn.Authenticator) Option {
	return func(o *options) error {
		o.auth = auth
		return nil
	}
}

// WithAuthFromKeychain is a functional option for overriding the default
// authenticator for remote operations, using an authn.Keychain to find
// credentials.
//
// The default authenticator is authn.Anonymous.
func WithAuthFromKeychain(keys authn.Keychain) Option {
	return func(o *options) error {
		o.keychain = keys
		return nil
	}
}

// WithPlatform is a functional option for overriding the default platform
// that Image and Descriptor.Image use for resolving an index to an image.
//
// The default platform is amd64/linux.
func WithPlatform(p v1.Platform) Option {
	return func(o *options) error {
		o.platform = p
		return nil
	}
}

// WithContext is a functional option for setting the context in http requests
// performed by a given function. Note that this context is used for _all_
// http requests, not just the initial volley. E.g., for remote.Image, the
// context will be set on http requests generated by subsequent calls to
// RawConfigFile() and even methods on layers returned by Layers().
//
// The default context is context.Background().
func WithContext(ctx context.Context) Option {
	return func(o *options) error {
		o.context = ctx
		return nil
	}
}

// WithJobs is a functional option for setting the parallelism of remote
// operations performed by a given function. Note that not all remote
// operations support parallelism.
//
// The default value is 4.
func WithJobs(jobs int) Option {
	return func(o *options) error {
		if jobs <= 0 {
			return errors.New("jobs must be greater than zero")
		}
		o.jobs = jobs
		return nil
	}
}

// WithUserAgent adds the given string to the User-Agent header for any HTTP
// requests. This header will also include "go-containerregistry/${version}".
//
// If you want to completely overwrite the User-Agent header, use WithTransport.
func WithUserAgent(ua string) Option {
	return func(o *options) error {
		o.userAgent = ua
		return nil
	}
}

// WithNondistributable includes non-distributable (foreign) layers
// when writing images, see:
// https://github.com/opencontainers/image-spec/blob/master/layer.md#non-distributable-layers
//
// The default behaviour is to skip these layers
func WithNondistributable(o *options) error {
	o.allowNondistributableArtifacts = true
	return nil
}

// WithProgress takes a channel that will receive progress updates as bytes are written.
//
// Sending updates to an unbuffered channel will block writes, so callers
// should provide a buffered channel to avoid potential deadlocks.
func WithProgress(updates chan<- v1.Update) Option {
	return func(o *options) error {
		o.updates = updates
		return nil
	}
}

// WithPageSize sets the given size as the value of parameter 'n' in the request.
//
// To omit the `n` parameter entirely, use WithPageSize(0).
// The default value is 1000.
func WithPageSize(size int) Option {
	return func(o *options) error {
		o.pageSize = size
		return nil
	}
}

// WithRetryBackoff sets the httpBackoff for retry HTTP operations.
func WithRetryBackoff(backoff Backoff) Option {
	return func(o *options) error {
		o.retryBackoff = backoff
		return nil
	}
}

// WithRetryPredicate sets the predicate for retry HTTP operations.
func WithRetryPredicate(predicate retry.Predicate) Option {
	return func(o *options) error {
		o.retryPredicate = predicate
		return nil
	}
}
