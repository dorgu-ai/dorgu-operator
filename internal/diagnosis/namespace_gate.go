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

// NamespaceGate decides whether a namespace is protected, meaning nothing about
// it may be sent to a model. It is the deny-list the remediation safety checker
// already enforces, passed in rather than copied so there is one decision. The
// interface lives here because remediation imports this package.
type NamespaceGate interface {
	// NamespaceProtected reports whether namespace is protected. It must fail
	// closed: an empty namespace, or a deny-list that cannot be read, is protected.
	NamespaceProtected(ctx context.Context, namespace string) bool
}
