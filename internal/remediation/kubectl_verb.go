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

package remediation

import "strings"

// Classifying a suggested kubectl command as a read or a write.
//
// Ownership shaping uses this to decide which commands a plan for an owned
// workload may keep: a direct write takes field ownership away from the owner,
// which is what makes the next `helm upgrade` fail outright rather than merely
// revert the fix (F-02).
//
// The parser and its tables are ported from dorgu-platform-v1
// (internal/view/step_command.go), which mirrors the CLI's. They are
// duplicated because no module imports another's internals; keep the three in
// step.
//
// Read-only verbs are listed explicitly rather than inferred from the absence
// of a mutating one, so a command whose verb matches nothing at all is refused
// rather than assumed harmless.
var (
	mutatingKubectlVerbs = map[string]bool{
		"annotate":  true,
		"apply":     true,
		"autoscale": true,
		"cordon":    true,
		"create":    true,
		"delete":    true,
		"drain":     true,
		"edit":      true,
		"expose":    true,
		"label":     true,
		"patch":     true,
		"replace":   true,
		"run":       true,
		"scale":     true,
		"set":       true,
		"taint":     true,
		"uncordon":  true,
	}

	readOnlyKubectlVerbs = map[string]bool{
		"api-resources": true,
		"api-versions":  true,
		"cluster-info":  true,
		"describe":      true,
		"diff":          true,
		"events":        true,
		"explain":       true,
		"get":           true,
		"logs":          true,
		"top":           true,
		"version":       true,
	}

	// readOnlyRolloutSubcommands are the `kubectl rollout` forms that only read.
	// undo, restart, pause and resume all write.
	readOnlyRolloutSubcommands = map[string]bool{
		"history": true,
		"status":  true,
	}

	// readOnlyAuthSubcommands are the `kubectl auth` forms that only read.
	// reconcile writes RBAC, so auth cannot be a read-only verb as a whole.
	readOnlyAuthSubcommands = map[string]bool{
		"can-i":  true,
		"whoami": true,
	}

	// verbsWithSubcommands are verbs whose effect depends on the subcommand
	// that follows them, mapped to the subcommands that only read.
	verbsWithSubcommands = map[string]map[string]bool{
		"auth":    readOnlyAuthSubcommands,
		"rollout": readOnlyRolloutSubcommands,
	}
)

// writesWorkload reports whether a suggested kubectl command may change
// cluster state: everything that is a kubectl invocation and is not positively
// recognised as read-only. A non-kubectl or empty command is not a workload
// write: it is already dropped by SanitizeStepCommand before it reaches here.
func writesWorkload(command string) bool {
	return isKubectlInvocation(command) && !readsOnly(command)
}

// isKubectlInvocation reports whether command names kubectl and something
// after it.
func isKubectlInvocation(command string) bool {
	fields := strings.Fields(command)
	return len(fields) >= 2 && fields[0] == "kubectl"
}

// Global kubectl flags, split by whether they consume the token after them.
//
// This table is the whole of the fix for a real bypass. The verb used to be
// found by scanning for the first token that matched a known verb name, which
// meant a flag VALUE that happened to be a verb name decided the answer:
//
//	kubectl -n logs delete deployment/foo
//
// classified as read-only, because "logs" was reached before "delete". A
// namespace called logs, get, top or describe is not exotic, and the
// consequence was a delete left in a plan for a workload Dorgu does not own,
// which is the exact failure ownership shaping exists to prevent.
//
// So the verb is now found positionally: flags are skipped, a value-taking flag
// takes its value with it, and the first token that is left is the verb. A flag
// this table does not know is not guessed at, because guessing is what the
// scan was doing.
var (
	// valueTakingGlobalFlags consume the next token.
	valueTakingGlobalFlags = map[string]bool{
		"-n": true, "--namespace": true,
		"-o": true, "--output": true,
		"-s": true, "--server": true,
		"--context":               true,
		"--cluster":               true,
		"--user":                  true,
		"--kubeconfig":            true,
		"--token":                 true,
		"--as":                    true,
		"--as-group":              true,
		"--as-uid":                true,
		"--cache-dir":             true,
		"--certificate-authority": true,
		"--client-certificate":    true,
		"--client-key":            true,
		"--request-timeout":       true,
		"--tls-server-name":       true,
		"--username":              true,
		"--password":              true,
		"--profile":               true,
		"--profile-output":        true,
		"--log-flush-frequency":   true,
		"-v":                      true,
		"--v":                     true,
		"--vmodule":               true,
	}

	// booleanGlobalFlags stand alone.
	booleanGlobalFlags = map[string]bool{
		"--insecure-skip-tls-verify": true,
		"--match-server-version":     true,
		"--warnings-as-errors":       true,
		"--disable-compression":      true,
	}
)

// readsOnly reports whether a kubectl command only reads cluster state.
//
// Anything not positively recognised as read-only is refused: an unknown verb,
// an unknown flag, a flag whose value is missing, or a command that names no
// verb at all. That direction is deliberate. Withholding a safe command costs the
// reader a copy-paste; handing over an unsafe one costs them a Deployment.
func readsOnly(command string) bool {
	verb, arguments, found := kubectlVerb(command)
	if !found {
		return false
	}
	if readOnlySubcommands, ok := verbsWithSubcommands[verb]; ok {
		return subcommandReadsOnly(arguments, readOnlySubcommands)
	}
	if mutatingKubectlVerbs[verb] {
		return false
	}
	return readOnlyKubectlVerbs[verb]
}

// kubectlVerb returns the subcommand a kubectl invocation names, and the tokens
// after it.
//
// It reports false rather than guessing whenever the command cannot be parsed
// with certainty: not a kubectl invocation, a flag this build does not know, a
// value-taking flag with nothing after it, or no verb at all.
func kubectlVerb(command string) (verb string, arguments []string, found bool) {
	fields := strings.Fields(command)
	if len(fields) < 2 || fields[0] != "kubectl" {
		return "", nil, false
	}

	for i := 1; i < len(fields); i++ {
		token := fields[i]

		if !strings.HasPrefix(token, "-") {
			// The first token that is not a flag is the verb.
			return token, fields[i+1:], true
		}

		// --flag=value carries its value, so nothing extra is skipped.
		if name, _, hasValue := strings.Cut(token, "="); hasValue {
			if !valueTakingGlobalFlags[name] && !booleanGlobalFlags[name] {
				return "", nil, false
			}
			continue
		}

		switch {
		case booleanGlobalFlags[token]:
			continue
		case valueTakingGlobalFlags[token]:
			// Its value is the next token and is not a verb. A flag with
			// nothing after it is a command this build cannot read.
			if i+1 >= len(fields) {
				return "", nil, false
			}
			i++
		default:
			// An unrecognised flag. Skipping it might skip a verb; consuming a
			// token after it might consume one. Neither guess is safe.
			return "", nil, false
		}
	}

	return "", nil, false
}

// subcommandReadsOnly classifies a verb that splits on its subcommand, such as
// `kubectl rollout` or `kubectl auth`, from the arguments after the verb.
//
// The subcommand must be the very next token. A flag in that position may take
// a value, and that value could be a read-only subcommand name hiding the real
// one (`kubectl rollout -n status restart`), so it is refused rather than
// skipped. A bare verb names no subcommand and is refused too.
func subcommandReadsOnly(arguments []string, readOnlySubcommands map[string]bool) bool {
	if len(arguments) == 0 || strings.HasPrefix(arguments[0], "-") {
		return false
	}
	return readOnlySubcommands[arguments[0]]
}
