/*
Copyright 2026.

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

package diagnosis

import "context"

type callOptionsKey struct{}

// CallOptions carries what the caller knows about one Analyze call and the
// providers cannot work out for themselves. It travels in the context because
// providers are built once at startup and the facts are per call.
type CallOptions struct {
	// NoModel withholds the analysed signals from every model call: providers that
	// would send them stay out of it and the rule-based diagnosis stands. The
	// reconciler sets it for groups whose incident has no persona to remediate
	// (unattributed), where an explanation would be paid for and never acted on.
	NoModel bool

	// PreviousOccurrences returns how many times the incident behind a diagnosis
	// has already been recorded, or 0 when it has not been. It lets the prompt
	// state the real history instead of none. It may be nil.
	PreviousOccurrences func(ctx context.Context, d Diagnosis) int32
}

// WithCallOptions returns a context carrying opts.
func WithCallOptions(ctx context.Context, opts CallOptions) context.Context {
	return context.WithValue(ctx, callOptionsKey{}, opts)
}

// CallOptionsFrom returns the options carried by ctx, or the zero value.
func CallOptionsFrom(ctx context.Context) CallOptions {
	opts, _ := ctx.Value(callOptionsKey{}).(CallOptions)
	return opts
}
