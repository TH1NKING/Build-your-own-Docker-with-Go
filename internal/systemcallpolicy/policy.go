// Package systemcallpolicy validates the operator-owned System Call Policy shared
// by Profile Bundle construction and the trusted Workload launch boundary.
package systemcallpolicy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const maximumPolicyBytes = 1 << 20

type Policy struct {
	Schema        string        `json:"schema"`
	Profile       string        `json:"profile"`
	Target        Target        `json:"target"`
	DefaultAction DefaultAction `json:"default_action"`
	Rules         []Rule        `json:"rules"`
}

type Target struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

type DefaultAction struct {
	Action string `json:"action"`
	Errno  int    `json:"errno"`
}

type Rule struct {
	Names     []string   `json:"names"`
	Action    string     `json:"action"`
	Arguments []Argument `json:"arguments"`
}

type Argument struct {
	Index    uint   `json:"index"`
	Operator string `json:"operator"`
	Value    uint64 `json:"value"`
	Mask     uint64 `json:"mask"`
}

func Parse(contents []byte) (Policy, error) {
	if len(contents) == 0 || len(contents) > maximumPolicyBytes {
		return Policy{}, errors.New("System Call Policy must contain between 1 and 1048576 bytes")
	}
	// encoding/json otherwise accepts repeated members (last value wins) and
	// case-insensitive field aliases. Neither belongs in a reviewed policy.
	tokens := json.NewDecoder(bytes.NewReader(contents))
	tokens.UseNumber()
	if err := scanJSONValue(tokens, 0); err != nil {
		return Policy{}, fmt.Errorf("decode System Call Policy: %w", err)
	}
	if _, err := tokens.Token(); err != io.EOF {
		return Policy{}, errors.New("System Call Policy must contain exactly one JSON value")
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var policy Policy
	if err := decoder.Decode(&policy); err != nil {
		return Policy{}, fmt.Errorf("decode System Call Policy: %w", err)
	}
	if err := policy.validate(); err != nil {
		return Policy{}, err
	}
	// This also checks the kernel instruction budget on non-Linux builders;
	// a successfully built bundle must not contain an unrepresentable policy.
	if _, err := compileInstructions(policy); err != nil {
		return Policy{}, err
	}
	return policy, nil
}

func Canonical(contents []byte) ([]byte, error) {
	policy, err := Parse(contents)
	if err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(policy)
	if err != nil {
		return nil, fmt.Errorf("encode canonical System Call Policy: %w", err)
	}
	return append(canonical, '\n'), nil
}

func (policy Policy) validate() error {
	if policy.Schema != "system-call-policy/v1" || policy.Profile != "python-data-v1" ||
		policy.Target.OS != "linux" || policy.Target.Arch != "amd64" {
		return errors.New("System Call Policy must target system-call-policy/v1 for python-data-v1 on linux/amd64")
	}
	if policy.DefaultAction.Action != "errno" || policy.DefaultAction.Errno < 1 || policy.DefaultAction.Errno > 4095 {
		return errors.New("System Call Policy default action must specify errno between 1 and 4095")
	}
	if policy.Rules == nil {
		return errors.New("System Call Policy rules must be an explicit JSON array")
	}
	lastRuleName := ""
	for ruleIndex, rule := range policy.Rules {
		if len(rule.Names) == 0 || rule.Action != "allow" {
			return errors.New("System Call Policy rules must name syscalls and use the allow action")
		}
		for nameIndex, name := range rule.Names {
			if _, exists := amd64SystemCalls[name]; !exists {
				return fmt.Errorf("unknown linux/amd64 System Call Policy syscall %q", name)
			}
			if nameIndex > 0 && rule.Names[nameIndex-1] >= name {
				return errors.New("System Call Policy syscall names must be unique and in bytewise order")
			}
		}
		if ruleIndex > 0 && lastRuleName >= rule.Names[0] {
			return errors.New("System Call Policy rules must be in bytewise syscall order")
		}
		lastRuleName = rule.Names[len(rule.Names)-1]
		if rule.Arguments == nil {
			return errors.New("System Call Policy rule arguments must be an explicit JSON array")
		}
		for argumentIndex, argument := range rule.Arguments {
			if argument.Index > 5 {
				return errors.New("System Call Policy argument index must be between 0 and 5")
			}
			if argumentIndex > 0 && rule.Arguments[argumentIndex-1].Index >= argument.Index {
				return errors.New("System Call Policy arguments must have unique increasing indexes")
			}
			switch argument.Operator {
			case "equal", "not-equal", "less-than", "less-or-equal", "greater-than", "greater-or-equal":
				if argument.Mask != 0 {
					return errors.New("System Call Policy argument mask is valid only with masked-equal")
				}
			case "masked-equal":
				if argument.Mask == 0 || argument.Value & ^argument.Mask != 0 {
					return errors.New("System Call Policy masked-equal requires a nonzero mask and a value contained in that mask")
				}
			default:
				return fmt.Errorf("unsupported System Call Policy argument operator %q", argument.Operator)
			}
		}
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("System Call Policy JSON nesting exceeds its limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]bool)
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := token.(string)
			if !ok || seen[name] {
				return fmt.Errorf("duplicate or invalid JSON member %q", name)
			}
			seen[name] = true
			switch name {
			case "schema", "profile", "target", "os", "arch", "default_action", "action", "errno", "rules", "names", "arguments", "index", "operator", "value", "mask":
			default:
				return fmt.Errorf("unknown JSON member %q", name)
			}
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	_, err = decoder.Token()
	return err
}
