package tools

import (
	"fmt"
	"regexp"
	"strings"
)

// defaultCTestPerTestTimeoutSec is injected into ctest invocations that omit
// --timeout. A hung gtest (TCP accept join, blocking recv) then fails the
// individual test instead of burning the whole sandbox/DoD wall-clock budget.
const defaultCTestPerTestTimeoutSec = 30

// ctestFilterTestFlag matches the non-existent flag weak models invent for
// leaf-scoped CTest DoD / run_bash ("ctest --filter-test cli"). Real CTest
// uses -R / --tests-regex.
var ctestFilterTestFlag = regexp.MustCompile(`--filter-test\s+(\S+)`)

// ctestHasTimeout reports whether a command already sets ctest's per-test
// --timeout (with space or = form).
var ctestHasTimeout = regexp.MustCompile(`(^|[\s])--timeout(=|\s|$)`)

// ctestInvoke matches a ctest binary token used as a command (bare or with a
// path prefix), including after shell separators.
var ctestInvoke = regexp.MustCompile(`(^|[\s;&|])((?:[^\s;|&]*/)?ctest)(\s|$)`)

// NormalizeCTestCommand rewrites invented/unsafe ctest CLI shapes that weak
// models emit into toolchain-correct form:
//   - --filter-test X  →  -R X
//   - inject --timeout N when missing so hung tests die quickly
//
// It is a no-op when cmd does not invoke ctest. Deliberately narrow: only
// flags that can never succeed as written, plus a safe per-test deadline.
func NormalizeCTestCommand(cmd string) string {
	if !strings.Contains(cmd, "ctest") {
		return cmd
	}
	if strings.Contains(cmd, "--filter-test") {
		cmd = ctestFilterTestFlag.ReplaceAllString(cmd, `-R $1`)
	}
	if !ctestHasTimeout.MatchString(cmd) {
		cmd = injectCTestTimeout(cmd, defaultCTestPerTestTimeoutSec)
	}
	return cmd
}

func injectCTestTimeout(cmd string, sec int) string {
	if sec <= 0 {
		sec = defaultCTestPerTestTimeoutSec
	}
	flag := fmt.Sprintf(" --timeout %d", sec)
	return ctestInvoke.ReplaceAllString(cmd, `${1}${2}`+flag+`${3}`)
}
