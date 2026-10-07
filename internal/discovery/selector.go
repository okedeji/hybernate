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

package discovery

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Selector is PromQL label matchers added to every history query, each
// written out again from its parts so nothing but a matcher reaches the
// query.
type Selector []string

// matcherOps are PromQL's label matching operators, the two-character ones
// first so "!=" isn't read as "!" and "=".
var matcherOps = []string{"=~", "!~", "!=", "="}

var labelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*`)

// ParseSelector reads label matchers as PromQL writes them inside braces,
// such as cluster="prod", replica=~"a|b", with the braces optional. Values
// are double-quoted or backquoted strings, and regular expressions must
// compile, as Prometheus would reject them otherwise.
func ParseSelector(s string) (Selector, error) {
	rest := strings.TrimSpace(s)
	if inner, ok := strings.CutPrefix(rest, "{"); ok {
		if inner, ok = strings.CutSuffix(inner, "}"); !ok {
			return nil, fmt.Errorf("selector %q opens a brace it doesn't close", s)
		}
		rest = inner
	}
	var out Selector
	for {
		rest = strings.TrimSpace(rest)
		if rest == "" {
			return out, nil
		}
		name := labelName.FindString(rest)
		if name == "" {
			return nil, fmt.Errorf("selector %q: expected a label name at %q", s, rest)
		}
		rest = strings.TrimSpace(rest[len(name):])
		op := ""
		for _, candidate := range matcherOps {
			if strings.HasPrefix(rest, candidate) {
				op = candidate
				break
			}
		}
		if op == "" {
			return nil, fmt.Errorf("selector %q: expected =, !=, =~ or !~ after %s", s, name)
		}
		rest = strings.TrimSpace(rest[len(op):])
		quoted, err := quotedPrefix(rest)
		if err != nil {
			return nil, fmt.Errorf("selector %q: the value of %s %w", s, name, err)
		}
		value, err := strconv.Unquote(quoted)
		if err != nil {
			return nil, fmt.Errorf("selector %q: the value of %s isn't a valid string: %w", s, name, err)
		}
		if op == "=~" || op == "!~" {
			if _, err := regexp.Compile("^(?:" + value + ")$"); err != nil {
				return nil, fmt.Errorf("selector %q: the value of %s isn't a valid regular expression: %w", s, name, err)
			}
		}
		out = append(out, name+op+strconv.Quote(value))
		rest = strings.TrimSpace(rest[len(quoted):])
		if rest != "" {
			after, ok := strings.CutPrefix(rest, ",")
			if !ok {
				return nil, fmt.Errorf("selector %q: expected a comma before %q", s, rest)
			}
			rest = after
		}
	}
}

// quotedPrefix is the double-quoted or backquoted string s starts with.
func quotedPrefix(s string) (string, error) {
	if s == "" || (s[0] != '"' && s[0] != '`') {
		return "", fmt.Errorf("must be a double-quoted string")
	}
	quote := s[0]
	for i := 1; i < len(s); i++ {
		switch {
		case s[i] == '\\' && quote == '"':
			i++
		case s[i] == quote:
			return s[:i+1], nil
		}
	}
	return "", fmt.Errorf("has no closing quote")
}
