package agent

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/esrrhs/divvy/pkg/models"
)

// planRepairMarker prefixes ErrorMsg when a leaf is being re-decomposed to
// fix a bad plan/DoD rather than because the work is too large. decompose
// uses it at MaxDepth to allow an atomic replan instead of forceLeaf.
const planRepairMarker = "[plan-repair]"

// planDiagnosisKind classifies why a leaf keeps failing after retries.
type planDiagnosisKind int

const (
	planKindUnknown planDiagnosisKind = iota
	planKindExpectedOutput
	planKindVerifyCommand
	planKindTooBig
)

type planDiagnosis struct {
	Kind    planDiagnosisKind
	Summary string
	// Constraint is injected into the node's contract for the next decompose.
	Constraint string
}

func (d planDiagnosis) IsPlanDefect() bool {
	return d.Kind == planKindExpectedOutput || d.Kind == planKindVerifyCommand
}

var expectedOutputMissing = regexp.MustCompile(`(?i)expected output "([^"]*)" not found`)

// diagnoseDecompositionFailure looks at a leaf's DoD and the latest failure
// text to decide whether the plan itself is wrong (especially an
// expected_output that the verification command never prints) versus the
// implementation being too large or simply broken.
func diagnoseDecompositionFailure(leaf *models.TaskNode, errMsg string) planDiagnosis {
	msg := strings.TrimSpace(errMsg)
	if leaf == nil {
		return planDiagnosis{Kind: planKindUnknown, Summary: "unknown failure", Constraint: msg}
	}
	dod := leaf.DoD
	want := strings.TrimSpace(dod.ExpectedOutput)

	if m := expectedOutputMissing.FindStringSubmatch(msg); len(m) == 2 {
		literal := m[1]
		cmds := strings.Join(dod.Commands, " ; ")
		summary := fmt.Sprintf(
			"DoD expected_output %q never appears in command output; commands=%q. "+
				"This is a decomposition defect: the acceptance check cannot pass as written.",
			literal, cmds)
		constraint := fmt.Sprintf(
			"%s Previous DoD is unsatisfiable: expected_output=%q with commands [%s]. "+
				"Prefer is_atomic=true. Fix the DoD: leave expected_output empty (trust exit codes), "+
				"or set it to text the command really prints (Go: default \"go test\" prints \"ok\", not \"PASS\"; "+
				"\"PASS\" needs \"go test -v\"). Do NOT only split into smaller tasks — the acceptance check itself is wrong.",
			planRepairMarker, literal, cmds)
		return planDiagnosis{Kind: planKindExpectedOutput, Summary: summary, Constraint: constraint}
	}

	if want != "" && strings.Contains(strings.ToLower(msg), "expected output") {
		summary := fmt.Sprintf("DoD expected_output %q likely mismatched with commands %v", want, dod.Commands)
		constraint := fmt.Sprintf(
			"%s Fix DoD expected_output/commands mismatch (want %q). Prefer is_atomic=true; leave expected_output empty unless a command prints that exact text.",
			planRepairMarker, want)
		return planDiagnosis{Kind: planKindExpectedOutput, Summary: summary, Constraint: constraint}
	}

	if strings.Contains(msg, "verify failed") || strings.Contains(msg, "exit ") ||
		strings.Contains(msg, "--- FAIL") || strings.Contains(msg, "build failed") {
		summary := "Verification command failed; implementation may be wrong or DoD commands may be unsuitable"
		constraint := fmt.Sprintf(
			"%s Previous verification failed. Re-check DoD commands and expected_output against real toolchain output, "+
				"then split only if the work itself is still too large. Last error: %s",
			planRepairMarker, truncate(msg, 800))
		return planDiagnosis{Kind: planKindVerifyCommand, Summary: summary, Constraint: constraint}
	}

	summary := "Leaf failed repeatedly; treat as possibly too large or poorly scoped"
	constraint := "Previous leaf execution failed; split into smaller tasks and address this error: " + truncate(msg, 1200)
	return planDiagnosis{Kind: planKindTooBig, Summary: summary, Constraint: constraint}
}
