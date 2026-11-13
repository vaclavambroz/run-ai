package cli

const Help = `Usage:
  run_ai [run] [OPTIONS] [DOMAIN]
  run_ai sessions list
  run_ai sessions terminate ID

Without a workflow selector run_ai opens the provider session for the current
host. DOMAIN is a compatibility alias for --dev DOMAIN.

Provider (at most one; default codex):
  --codex                    use Codex
  --claude                   use Claude
  --gemini                   use Gemini through the Antigravity runtime
  --provider NAME            codex, claude, gemini, or antigravity

Access mode (default full):
  --confirm                  planned per-action confirmation; this build
                             fails closed before launch
  --readonly                 planned hard read-only isolation; this build
                             fails closed before launch

  WARNING: the default full mode preserves the current unrestricted launcher
  behavior. Monitoring cannot use --readonly. Spam with --readonly is
  analyze-only and never changes rules or mailboxes.

Workflow (at most one kind):
  --monitoring               plan Icinga repair and robot ownership/cleanup;
                             plan-only in this build
  --spam URL [URL ...]       analyze, narrowly regress and (outside readonly)
                             plan antispam; plan-only in this build, max 10
  --dev DOMAIN               resolve the domain inventory and development root

Collaboration:
  --team                     activate the fixed Turing/Fermat/Dewey/Faraday
                             team contract in a new isolated local session;
                             --resume-id requires its exact durable lineage

Session:
  --new-session              create an isolated tmux/provider lineage
  --resume-id ID             find and resume one exact provider lineage
  --persistent               use the reconnectable Codex app-server supervisor
                             (Codex/full only; --team is intentionally rejected)
  sessions list              list classic and persistent session metadata
  sessions terminate ID      stop one exact verified live session (metadata is retained)

  Claude lineages carry an exact provider ID and can be resumed by ID.
  Fresh Codex sessions bind their exact local thread ID after attach returns.
  Successful capture permits --resume-id; an explicit warning means the
  lineage remains tmux_lifetime_only. Gemini remains tmux_lifetime_only.

  Persistent constraints: local Codex/full only. Claude, Gemini, --team,
  --confirm, --readonly, remote --dev and plan-only monitoring/spam are not
  silently converted into persistent sessions.

  Persistent terminal: enter a short prompt on one line. For Markdown or
  multiline input use /paste ... /endpaste, or /prompt-file /absolute/path.

Model:
  --model MODEL              select one exact model identifier
  --default                  use the maintained compatibility baseline
  --latest                   use the maintained current model

Other:
  --explain                  print the normalized request without launching AI
  -h, --help

Examples:
  run_ai --codex
  run_ai --claude --new-session
  run_ai --claude --confirm --new-session --explain
  run_ai --gemini --dev example.cz
  run_ai --codex --monitoring --explain
  run_ai --codex --team --dev example.cz
  run_ai --codex --persistent --new-session
  run_ai --codex --persistent --resume-id persistent-EXACT-ID
  run_ai --claude --spam 'https://spam.example.com/email/view?id=123&status=active' --explain
  run_ai --codex --readonly --dev example.cz --explain

Exit status:
  0 success, 2 invalid request, 69 unavailable safety prerequisite,
  70 execution failure
`
