package cli

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/vaclavambroz/run-ai/internal/permissions"
	"github.com/vaclavambroz/run-ai/internal/providers"
	"github.com/vaclavambroz/run-ai/internal/sessions"
	"github.com/vaclavambroz/run-ai/internal/workflows"
)

type ParseError struct{ Message string }

func (e *ParseError) Error() string { return e.Message }

type parseState struct {
	provider         providers.Name
	providerAlias    string
	providerExplicit bool
	confirm          bool
	readonly         bool
	monitoring       bool
	team             bool
	spamURLs         []string
	spamHost         string
	devDomain        string
	workflowSelected string
	newSession       bool
	resumeID         string
	persistent       bool
	modelTrack       ModelTrack
	model            string
	explain          bool
	help             bool
}

func Parse(args []string) (Request, error) {
	if len(args) == 2 && args[0] == "sessions" && args[1] == "list" {
		return Request{ListSessions: true}, nil
	}
	if len(args) == 3 && args[0] == "sessions" && args[1] == "terminate" {
		if err := sessions.ValidateID(args[2]); err != nil {
			return Request{}, usageError(fmt.Sprintf("invalid session ID: %v", err))
		}
		return Request{TerminateSessionID: args[2]}, nil
	}
	if len(args) > 0 && args[0] == "sessions" {
		return Request{}, usageError("sessions supports only 'sessions list' or 'sessions terminate ID'")
	}
	state := parseState{provider: providers.Codex, providerAlias: "configured_default", modelTrack: ModelConfigured}
	if len(args) > 0 && args[0] == "run" {
		args = args[1:]
	}
	positionalOnly := false
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if positionalOnly {
			if err := state.addPositional(arg); err != nil {
				return Request{}, err
			}
			continue
		}
		if arg == "--" {
			positionalOnly = true
			continue
		}
		switch arg {
		case "-h", "--help":
			state.help = true
		case "--explain":
			state.explain = true
		case "--codex":
			if err := state.setProvider(providers.Codex, "codex", "--codex"); err != nil {
				return Request{}, err
			}
		case "--claude":
			if err := state.setProvider(providers.Claude, "claude", "--claude"); err != nil {
				return Request{}, err
			}
		case "--gemini":
			if err := state.setProvider(providers.Antigravity, "gemini", "--gemini"); err != nil {
				return Request{}, err
			}
		case "--provider":
			value, next, err := requireValue(args, index, arg)
			if err != nil {
				return Request{}, err
			}
			index = next
			provider, err := providers.Parse(value)
			if err != nil {
				return Request{}, usageError(err.Error())
			}
			if err := state.setProvider(provider, strings.ToLower(strings.TrimSpace(value)), "--provider"); err != nil {
				return Request{}, err
			}
		case "--confirm":
			state.confirm = true
		case "--readonly":
			state.readonly = true
		case "--monitoring":
			if err := state.setMonitoring(arg); err != nil {
				return Request{}, err
			}
		case "--team":
			state.team = true
		case "--spam-host":
			value, next, err := requireValue(args, index, arg)
			if err != nil {
				return Request{}, err
			}
			index = next
			if err := validateOpaqueValue(arg, value); err != nil {
				return Request{}, err
			}
			state.spamHost = value
		case "--spam":
			value, next, err := requireValue(args, index, arg)
			if err != nil {
				return Request{}, err
			}
			index = next
			if err := state.addSpam(value); err != nil {
				return Request{}, err
			}
			// Compatibility accepts both repeated --spam flags and one --spam
			// followed by several URLs. Stop at the next option so workflow and
			// global flags still receive normal conflict validation.
			for index+1 < len(args) && !strings.HasPrefix(args[index+1], "-") {
				index++
				if err := state.addSpam(args[index]); err != nil {
					return Request{}, err
				}
			}
		case "--dev":
			value, next, err := requireValue(args, index, arg)
			if err != nil {
				return Request{}, err
			}
			index = next
			if err := validateOpaqueValue(arg, value); err != nil {
				return Request{}, err
			}
			if err := state.setDev(value, arg); err != nil {
				return Request{}, err
			}
		case "--new-session":
			state.newSession = true
		case "--persistent":
			state.persistent = true
		case "--resume-id":
			value, next, err := requireValue(args, index, arg)
			if err != nil {
				return Request{}, err
			}
			index = next
			state.resumeID = value
		case "--model":
			value, next, err := requireValue(args, index, arg)
			if err != nil {
				return Request{}, err
			}
			index = next
			if err := state.setModel(ModelExplicit, value, arg); err != nil {
				return Request{}, err
			}
		case "--default":
			if err := state.setModel(ModelDefault, "", arg); err != nil {
				return Request{}, err
			}
		case "--latest":
			if err := state.setModel(ModelLatest, "", arg); err != nil {
				return Request{}, err
			}
		default:
			if strings.HasPrefix(arg, "--provider=") {
				value := strings.TrimPrefix(arg, "--provider=")
				provider, err := providers.Parse(value)
				if err != nil {
					return Request{}, usageError(err.Error())
				}
				if err := state.setProvider(provider, strings.ToLower(strings.TrimSpace(value)), "--provider"); err != nil {
					return Request{}, err
				}
				continue
			}
			if strings.HasPrefix(arg, "--resume-id=") {
				state.resumeID = strings.TrimPrefix(arg, "--resume-id=")
				continue
			}
			if strings.HasPrefix(arg, "--model=") {
				if err := state.setModel(ModelExplicit, strings.TrimPrefix(arg, "--model="), "--model"); err != nil {
					return Request{}, err
				}
				continue
			}
			if strings.HasPrefix(arg, "--spam=") {
				if err := state.addSpam(strings.TrimPrefix(arg, "--spam=")); err != nil {
					return Request{}, err
				}
				continue
			}
			if strings.HasPrefix(arg, "--dev=") {
				parts := strings.SplitN(arg, "=", 2)
				if err := validateOpaqueValue(parts[0], parts[1]); err != nil {
					return Request{}, err
				}
				if err := state.setDev(parts[1], parts[0]); err != nil {
					return Request{}, err
				}
				continue
			}
			if strings.HasPrefix(arg, "-") {
				return Request{}, usageError(fmt.Sprintf("unknown option %q", arg))
			}
			if err := state.addPositional(arg); err != nil {
				return Request{}, err
			}
		}
	}

	if state.newSession && state.resumeID != "" {
		return Request{}, usageError("--new-session and --resume-id are mutually exclusive")
	}
	if state.resumeID != "" {
		if err := sessions.ValidateID(state.resumeID); err != nil {
			return Request{}, usageError(fmt.Sprintf("invalid --resume-id: %v", err))
		}
	}
	if state.persistent && state.provider != providers.Codex {
		return Request{}, usageError("--persistent is supported only with Codex")
	}
	if state.persistent && state.team {
		return Request{}, usageError("--persistent --team is not supported: the app-server supervisor does not provide the required independent team-agent contract")
	}
	mode, err := permissions.Resolve(state.confirm, state.readonly)
	if err != nil {
		return Request{}, usageError(err.Error())
	}
	policy, err := permissions.PolicyFor(mode)
	if err != nil {
		return Request{}, err
	}
	var plan workflows.Plan
	if len(state.spamURLs) > 0 {
		if state.spamHost == "" {
			return Request{}, usageError("--spam requires --spam-host to specify the target mail host (e.g. --spam-host mail.example.com)")
		}
		spamProfile := workflows.DefaultProfile()
		spamProfile.Spam.Host = state.spamHost
		var buildErr error
		plan, buildErr = workflows.BuildWithProfile(workflows.Input{
			AccessMode: mode,
			SpamURLs:   append([]string(nil), state.spamURLs...),
			Team:       state.team,
		}, spamProfile)
		if buildErr != nil {
			return Request{}, usageError(buildErr.Error())
		}
	} else {
		var buildErr error
		plan, buildErr = workflows.Build(workflows.Input{
			AccessMode: mode,
			Monitoring: state.monitoring,
			DevDomain:  state.devDomain,
			Team:       state.team,
		})
		if buildErr != nil {
			return Request{}, usageError(buildErr.Error())
		}
	}
	if plan.RequiresNewSession {
		if state.resumeID != "" && !state.team {
			return Request{}, usageError("selected workflow requires --new-session and conflicts with --resume-id")
		}
		if state.resumeID == "" {
			state.newSession = true
		}
	}
	return Request{
		Provider: state.provider, ProviderAlias: state.providerAlias, ProviderExplicit: state.providerExplicit,
		AccessMode: mode, AccessPolicy: policy,
		Workflow: plan,
		Session:  SessionOptions{NewSession: state.newSession, ResumeID: state.resumeID, Persistent: state.persistent},
		Model:    ModelOptions{Track: state.modelTrack, Model: state.model},
		Help:     state.help, Explain: state.explain,
	}, nil
}

func (s *parseState) setProvider(provider providers.Name, alias, flag string) error {
	if s.providerExplicit {
		return usageError(fmt.Sprintf("provider selector %s conflicts with an earlier provider selector", flag))
	}
	s.provider = provider
	s.providerAlias = alias
	s.providerExplicit = true
	return nil
}

func (s *parseState) setMonitoring(flag string) error {
	if s.workflowSelected != "" {
		return usageError(fmt.Sprintf("workflow selector %s conflicts with %s workflow", flag, s.workflowSelected))
	}
	s.monitoring = true
	s.workflowSelected = "monitoring"
	return nil
}

func (s *parseState) addSpam(value string) error {
	if err := validateOpaqueValue("--spam", value); err != nil {
		return err
	}
	if s.workflowSelected != "" && s.workflowSelected != "spam" {
		return usageError(fmt.Sprintf("workflow selector --spam conflicts with %s workflow", s.workflowSelected))
	}
	s.workflowSelected = "spam"
	s.spamURLs = append(s.spamURLs, value)
	return nil
}

func (s *parseState) setDev(value, flag string) error {
	if s.workflowSelected != "" {
		return usageError(fmt.Sprintf("workflow selector %s conflicts with %s workflow", flag, s.workflowSelected))
	}
	s.workflowSelected = "dev"
	s.devDomain = value
	return nil
}

func (s *parseState) addPositional(value string) error {
	if err := validateOpaqueValue("DOMAIN", value); err != nil {
		return err
	}
	if s.workflowSelected != "" {
		return usageError(fmt.Sprintf("positional DOMAIN conflicts with %s workflow", s.workflowSelected))
	}
	return s.setDev(value, "positional DOMAIN")
}

func (s *parseState) setModel(track ModelTrack, model, flag string) error {
	if s.modelTrack != ModelConfigured {
		return usageError(fmt.Sprintf("model selector %s conflicts with earlier %s selection", flag, s.modelTrack))
	}
	if track == ModelExplicit {
		if err := validateOpaqueValue("--model", model); err != nil {
			return err
		}
	}
	s.modelTrack = track
	s.model = model
	return nil
}

func requireValue(args []string, index int, flag string) (string, int, error) {
	if index+1 >= len(args) || args[index+1] == "" || strings.HasPrefix(args[index+1], "--") {
		return "", index, usageError(fmt.Sprintf("%s requires a value", flag))
	}
	return args[index+1], index + 1, nil
}

func validateOpaqueValue(flag, value string) error {
	if value == "" {
		return usageError(fmt.Sprintf("%s requires a non-empty value", flag))
	}
	if len(value) > 4096 {
		return usageError(fmt.Sprintf("%s value is too long", flag))
	}
	for _, r := range value {
		if unicode.IsControl(r) || r == '\x00' {
			return usageError(fmt.Sprintf("%s value contains a control character", flag))
		}
	}
	return nil
}

func usageError(message string) error { return &ParseError{Message: message} }

func IsParseError(err error) bool {
	var parseError *ParseError
	return errors.As(err, &parseError)
}
