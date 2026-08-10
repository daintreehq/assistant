package scenario

import (
	"fmt"
	"time"

	"github.com/daintreehq/assistant/benchmarks/orchestration/world"
)

// All returns the full scenario suite: the ~10 core workflows the assistant
// runs constantly, plus fault variants reproducing the Daintree quirks that
// have caused real failures. Nonce tokens (PATCH_ID=…, token=…) exist ONLY
// inside the fake world's agent output — a final answer containing one proves
// the orchestrator actually read the world, end to end.
func All() []Scenario {
	return []Scenario{
		statusOverview(),
		readAgentReport(),
		spawnFixAndReport(),
		spawnDontWait(),
		twoAgentCohort(),
		relayQuestionAnswer(),
		failureHonesty(),
		worktreePickByBranch(),
		summarizeTwoTerminals(),
		noWorkInvented(),
		closeOnRequest(),
		blankTailDeepRead(),
		hungAgentNoStall(),
		fastFinisherRelay(),
		throttledReadsRecover(),
		questionSurfacedNotHung(),
		latencyChat(),
		latencyToolRead(),
	}
}

// backdated returns a spawn instant far enough in the past that a pre-seeded
// terminal's bare "waiting" is old enough to settle without a seen-working tick.
func backdated() time.Time { return time.Now().Add(-5 * time.Minute) }

// --- core: status & reads ----------------------------------------------------

func statusOverview() Scenario {
	return Scenario{
		ID:       "status-overview",
		Category: "status",
		Prompt:   "Give me a quick status of everything running right now.",
		Timeout:  2 * time.Minute,
		Setup: func(w *world.World) {
			w.AddTerminal(world.Terminal{
				ID: "terminal-alpha-tests", Name: "Claude: alpha test sweep", AgentID: "claude",
				SpawnedAt: backdated(),
				Script: world.Script{Phases: []world.Phase{
					{After: 0, State: "working", Append: "running test sweep...\nsuite 3/9 in progress\n"},
				}},
			})
			w.AddTerminal(world.Terminal{
				ID: "terminal-beta-migration", Name: "Claude: beta schema migration", AgentID: "claude",
				SpawnedAt: backdated(),
				Script: world.Script{Phases: []world.Phase{
					{After: 0, State: "waiting", WaitingReason: "prompt", Append: "Schema migration finished cleanly: 12 tables updated.\n"},
				}},
			})
			w.AddTerminal(world.Terminal{
				ID: "terminal-gamma-docs", Name: "Codex: gamma docs update", AgentID: "codex",
				SpawnedAt: backdated(),
				Script: world.Script{Phases: []world.Phase{
					{After: 0, State: "waiting", WaitingReason: "question", Append: "Drafted the API docs. Should I use British or American spelling?\n"},
				}},
			})
		},
		Checks: []Check{
			ResultSuccess(),
			WorldCalledAny(1, "terminal.list", "terminal.getStatus"),
			SpawnCount(0),
			WorldNotCalled("terminal.close"),
			AnswerContains("alpha"),
			AnswerContains("beta"),
			AnswerContains("gamma"),
		},
		Notes: "Pure read: report the live roster without touching anything.",
	}
}

func readAgentReport() Scenario {
	return Scenario{
		ID:       "read-agent-report",
		Category: "extract",
		Prompt:   "The migration agent in the migration-runner terminal should be done — what did it report? How many migrations were applied?",
		Timeout:  3 * time.Minute,
		Setup: func(w *world.World) {
			w.AddTerminal(world.Terminal{
				ID: "terminal-migration-runner", Name: "Claude: migration-runner", AgentID: "claude",
				SpawnedAt: backdated(),
				Script: world.Script{Phases: []world.Phase{
					{After: 0, State: "waiting", WaitingReason: "prompt", Append: "Applying migrations...\n0001_init OK\n0002_users OK\n...\nMigration complete. MIGRATION_COUNT=37 applied, 0 failures.\n"},
				}},
			})
		},
		Checks: []Check{
			ResultSuccess(),
			WorldCalledAny(1, "terminal.getOutput", "terminal.getStatus"),
			SpawnCount(0),
			AnswerContains("37"),
		},
		Notes: "Read a finished terminal's output and relay the concrete result.",
	}
}

func summarizeTwoTerminals() Scenario {
	return Scenario{
		ID:       "summarize-two-terminals",
		Category: "extract",
		Prompt:   "Check the test-runner and lint-runner terminals and give me a combined report — how many tests failed, which tests exactly, and is lint clean?",
		Timeout:  3 * time.Minute,
		Setup: func(w *world.World) {
			w.AddTerminal(world.Terminal{
				ID: "terminal-test-runner", Name: "Claude: test-runner", AgentID: "claude",
				SpawnedAt: backdated(),
				Script: world.Script{Phases: []world.Phase{
					{After: 0, State: "waiting", WaitingReason: "prompt", Append: "go test ./...\n...\nTests: 42 passed, 3 failed.\nFailing: auth_test.go::TestRedirect, auth_test.go::TestCallback, session_test.go::TestExpiry\n"},
				}},
			})
			w.AddTerminal(world.Terminal{
				ID: "terminal-lint-runner", Name: "Claude: lint-runner", AgentID: "claude",
				SpawnedAt: backdated(),
				Script: world.Script{Phases: []world.Phase{
					{After: 0, State: "waiting", WaitingReason: "prompt", Append: "golangci-lint run ./...\nLint clean: 0 issues in 214 files.\n"},
				}},
			})
		},
		Checks: []Check{
			ResultSuccess(),
			SpawnCount(0),
			AnswerContains("42"),
			AnswerMatches("(?i)(auth_test|redirect)"),
			AnswerMatches("(?i)(clean|0 issues|no issues)"),
		},
		Notes: "Multi-terminal read + merge into one coherent report.",
	}
}

func noWorkInvented() Scenario {
	return Scenario{
		ID:       "no-work-invented",
		Category: "status",
		Prompt:   "Is anything running right now? Don't start anything — just check.",
		Timeout:  90 * time.Second,
		Setup: func(w *world.World) {
			w.Worktrees = []world.Worktree{{ID: "wt-main", Path: "/Users/bench/project", Branch: "main"}}
		},
		Checks: []Check{
			ResultSuccess(),
			SpawnCount(0),
			WorldNotCalled("agent.launch"),
			WorldCalledAny(1, "terminal.list", "terminal.getStatus"),
			AnswerMatches(`(?i)\b(nothing|no|none|idle|not)\b`),
		},
		Notes: "Empty world: the assistant must report quiet after actually checking, not invent work.",
	}
}

// --- core: spawn & supervise ---------------------------------------------------

func spawnFixAndReport() Scenario {
	return Scenario{
		ID:       "spawn-fix-and-report",
		Category: "spawn",
		Prompt:   "Spawn an agent in the fix-auth worktree to fix the login redirect bug — after login, users land on /home instead of their original destination. Wait for it to finish, tell me what it changed, and give me the patch id it prints when done.",
		Timeout:  5 * time.Minute,
		Setup: func(w *world.World) {
			w.Worktrees = []world.Worktree{
				{ID: "wt-fix-auth", Path: "/Users/bench/worktrees/fix-auth", Branch: "fix-auth"},
				{ID: "wt-main", Path: "/Users/bench/project", Branch: "main"},
			}
			w.SetSpawnScript(func(agentID, worktreeID, prompt string) world.Script {
				return world.Script{Phases: []world.Phase{
					{After: 0, State: "working", Append: "Reading auth/session.ts...\n"},
					{After: 8 * time.Second, State: "working", Append: "Found the bug: redirect target dropped in handleCallback().\nApplying fix...\n"},
					{After: 22 * time.Second, State: "waiting", WaitingReason: "prompt", Append: "Fixed the redirect in auth/session.ts — the original destination is now preserved through the login flow. Tests pass. PATCH_ID=9f3a2c\n"},
				}}
			})
		},
		Checks: []Check{
			ResultSuccess(),
			SpawnCount(1),
			SpawnedInWorktree("wt-fix-auth"),
			AnswerContains("9f3a2c"),
		},
		Notes: "THE core workflow: spawn in the right worktree, supervise in-turn, report the agent's actual result.",
	}
}

func spawnDontWait() Scenario {
	return Scenario{
		ID:       "spawn-dont-wait",
		Category: "spawn",
		Prompt:   "Kick off an agent in the perf worktree to profile the slow /search endpoint — don't wait for it to finish, just confirm it's started.",
		Timeout:  3 * time.Minute,
		Setup: func(w *world.World) {
			w.Worktrees = []world.Worktree{
				{ID: "wt-perf", Path: "/Users/bench/worktrees/perf", Branch: "perf"},
			}
			w.SetSpawnScript(func(agentID, worktreeID, prompt string) world.Script {
				phases := []world.Phase{{After: 0, State: "working", Append: "Setting up profiler...\n"}}
				for i := 1; i <= 20; i++ {
					phases = append(phases, world.Phase{
						After: time.Duration(i) * 30 * time.Second, State: "working",
						Append: "profiling batch...\n",
					})
				}
				return world.Script{Phases: phases}
			})
		},
		Checks: []Check{
			ResultSuccess(),
			SpawnCount(1),
			SpawnedInWorktree("wt-perf"),
			Under(2 * time.Minute),
			AnswerMatches("(?i)(started|kicked|launch|running|spawned|under ?way|on it)"),
		},
		Notes: "Fire-and-acknowledge: must NOT sit through the agent's whole run when told not to wait.",
	}
}

func twoAgentCohort() Scenario {
	return Scenario{
		ID:       "two-agent-cohort",
		Category: "spawn",
		Prompt:   "Spawn two agents in parallel: one in the api worktree to add rate limiting to the public endpoints, and one in the ui worktree to fix the dark-mode contrast issues. Wait for both to finish and summarize what each one did — include the completion token each agent prints.",
		Timeout:  6 * time.Minute,
		Setup: func(w *world.World) {
			w.Worktrees = []world.Worktree{
				{ID: "wt-api", Path: "/Users/bench/worktrees/api", Branch: "api"},
				{ID: "wt-ui", Path: "/Users/bench/worktrees/ui", Branch: "ui"},
			}
			w.SetSpawnScript(func(agentID, worktreeID, prompt string) world.Script {
				if worktreeID == "wt-api" {
					return world.Script{Phases: []world.Phase{
						{After: 0, State: "working", Append: "Adding rate limiting middleware...\n"},
						{After: 18 * time.Second, State: "waiting", WaitingReason: "prompt", Append: "Rate limiting added to all public endpoints (100 req/min, sliding window). token=RL-77ac1\n"},
					}}
				}
				return world.Script{Phases: []world.Phase{
					{After: 0, State: "working", Append: "Auditing dark-mode contrast...\n"},
					{After: 30 * time.Second, State: "waiting", WaitingReason: "prompt", Append: "Fixed 6 contrast violations; all text now meets WCAG AA in dark mode. token=UI-b33d9\n"},
				}}
			})
		},
		Checks: []Check{
			ResultSuccess(),
			SpawnCount(2),
			SpawnedInWorktree("wt-api"),
			SpawnedInWorktree("wt-ui"),
			AnswerContains("77ac1"),
			AnswerContains("b33d9"),
		},
		Notes: "Parallel fan-out + cohort await + merged per-agent report.",
	}
}

func failureHonesty() Scenario {
	return Scenario{
		ID:       "failure-honesty",
		Category: "spawn",
		Prompt:   "Spawn an agent in the payments worktree to upgrade the Stripe SDK to v12, wait for it, and tell me how it went — if anything fails, give me the exact error code.",
		Timeout:  5 * time.Minute,
		Setup: func(w *world.World) {
			w.Worktrees = []world.Worktree{
				{ID: "wt-payments", Path: "/Users/bench/worktrees/payments", Branch: "payments"},
			}
			w.SetSpawnScript(func(agentID, worktreeID, prompt string) world.Script {
				return world.Script{Phases: []world.Phase{
					{After: 0, State: "working", Append: "Upgrading stripe to v12...\n"},
					{After: 15 * time.Second, State: "exited", ExitCode: world.IntPtr(1), Append: "FATAL: peer dependency conflict: stripe@12 requires node >= 20 (found 18.17). Aborting. ERROR_CODE=E-DEP-4402\n"},
				}}
			})
		},
		Checks: []Check{
			ResultSuccess(),
			SpawnCount(1),
			AnswerContains("E-DEP-4402"),
			AnswerMatches("(?i)(fail|error|abort|conflict|couldn't|could not|blocked)"),
		},
		Notes: "The agent dies with a nonzero exit: the report must be honest, specific, and not claim success.",
	}
}

func worktreePickByBranch() Scenario {
	return Scenario{
		ID:       "worktree-pick-by-branch",
		Category: "spawn",
		Prompt:   "Start an agent on the feature/payments branch worktree to add invoice PDF export, wait for it, and report back with the completion token it prints.",
		Timeout:  5 * time.Minute,
		Setup: func(w *world.World) {
			w.Worktrees = []world.Worktree{
				{ID: "wt-001", Path: "/Users/bench/project", Branch: "main"},
				{ID: "wt-002", Path: "/Users/bench/worktrees/payments", Branch: "feature/payments"},
			}
			w.SetSpawnScript(func(agentID, worktreeID, prompt string) world.Script {
				return world.Script{Phases: []world.Phase{
					{After: 0, State: "working", Append: "Building invoice PDF exporter...\n"},
					{After: 15 * time.Second, State: "waiting", WaitingReason: "prompt", Append: "Invoice PDF export shipped behind the /invoices/:id/pdf route. token=INV-5521\n"},
				}}
			})
		},
		Checks: []Check{
			ResultSuccess(),
			SpawnCount(1),
			SpawnedInWorktree("wt-002"),
			AnswerContains("5521"),
		},
		Notes: "The user names a BRANCH; the spawn must land in that branch's worktree id (the silent-null spawn bug class).",
	}
}

// --- core: interaction ---------------------------------------------------------

func relayQuestionAnswer() Scenario {
	return Scenario{
		ID:       "relay-question-answer",
		Category: "interact",
		Prompt:   "The deploy agent asked a question — tell it to use staging, then wait for it to finish and tell me the deployed revision it reports.",
		Timeout:  5 * time.Minute,
		Setup: func(w *world.World) {
			w.AddTerminal(world.Terminal{
				ID: "terminal-deploy-agent", Name: "Claude: deploy-agent", AgentID: "claude",
				SpawnedAt: backdated(),
				Script: world.Script{
					Phases: []world.Phase{
						{After: 0, State: "working", Append: "Preparing deployment bundle...\n"},
						{After: 30 * time.Second, State: "waiting", WaitingReason: "question", Append: "Bundle ready. Which environment should I target? (staging/production)\n"},
					},
					OnInput: []world.Phase{
						{After: 0, State: "working", Append: "Deploying to staging...\n"},
						{After: 8 * time.Second, State: "waiting", WaitingReason: "prompt", Append: "Deployment complete. DEPLOY_URL=https://staging.example.dev rev=77ffe2\n"},
					},
				},
			})
		},
		Checks: []Check{
			ResultSuccess(),
			WorldCalled("terminal.sendCommand", 1),
			InputSent("staging"),
			AnswerContains("77ffe2"),
			SpawnCount(0),
		},
		Notes: "Answer a waiting agent's question via sendCommand, then supervise it to completion.",
	}
}

func closeOnRequest() Scenario {
	return Scenario{
		ID:       "close-on-request",
		Category: "interact",
		Prompt:   "Please close the old lint-runner terminal — it's done and I don't need it anymore.",
		Timeout:  2 * time.Minute,
		Setup: func(w *world.World) {
			w.AddTerminal(world.Terminal{
				ID: "terminal-lint-runner", Name: "Claude: lint-runner", AgentID: "claude",
				SpawnedAt: backdated(),
				Script: world.Script{Phases: []world.Phase{
					{After: 0, State: "waiting", WaitingReason: "prompt", Append: "Lint clean: 0 issues.\n"},
				}},
			})
			w.AddTerminal(world.Terminal{
				ID: "terminal-active-work", Name: "Claude: active feature work", AgentID: "claude",
				SpawnedAt: backdated(),
				Script: world.Script{Phases: []world.Phase{
					{After: 0, State: "working", Append: "implementing...\n"},
				}},
			})
		},
		Checks: []Check{
			ResultSuccess(),
			TerminalClosed("terminal-lint-runner"),
			Check{Name: "other terminal untouched", Fn: func(r *RunResult) error {
				if r.World.IsClosed("terminal-active-work") {
					return fmt.Errorf("the active-work terminal was closed too — only lint-runner was requested")
				}
				return nil
			}},
			SpawnCount(0),
		},
		Notes: "Close exactly the requested terminal — and never the other one (a real past incident).",
	}
}

// --- fault variants -------------------------------------------------------------

func blankTailDeepRead() Scenario {
	return Scenario{
		ID:       "blank-tail-deep-read",
		Category: "fault",
		Prompt:   "Spawn an agent in the fix-auth worktree to fix the login redirect bug, wait for it to finish, and tell me what it changed — include the patch id it prints when done.",
		Timeout:  5 * time.Minute,
		Setup: func(w *world.World) {
			w.Faults.BlankStatusTail = true
			w.Worktrees = []world.Worktree{
				{ID: "wt-fix-auth", Path: "/Users/bench/worktrees/fix-auth", Branch: "fix-auth"},
			}
			w.SetSpawnScript(func(agentID, worktreeID, prompt string) world.Script {
				return world.Script{Phases: []world.Phase{
					{After: 0, State: "working", Append: "Investigating redirect flow...\n"},
					{After: 20 * time.Second, State: "waiting", WaitingReason: "prompt", Append: "Redirect bug fixed in auth/session.ts; destination preserved. PATCH_ID=77b1e4\n"},
				}}
			})
		},
		Checks: []Check{
			ResultSuccess(),
			SpawnCount(1),
			WorldCalled("terminal.getOutput", 1),
			AnswerContains("77b1e4"),
		},
		Notes: "Codex-style bottom-padded TUI: the inline status tail is all whitespace; only the deep getOutput has the real content.",
	}
}

func hungAgentNoStall() Scenario {
	return Scenario{
		ID:       "hung-agent-no-stall",
		Category: "fault",
		Prompt:   "Spawn an agent in the refactor worktree to rename the User model to Account across the codebase, wait for it, and report.",
		Timeout:  6 * time.Minute,
		Setup: func(w *world.World) {
			w.Worktrees = []world.Worktree{
				{ID: "wt-refactor", Path: "/Users/bench/worktrees/refactor", Branch: "refactor"},
			}
			w.SetSpawnScript(func(agentID, worktreeID, prompt string) world.Script {
				phases := []world.Phase{{After: 0, State: "working", Append: "Scanning for references...\n"}}
				for i := 1; i <= 40; i++ {
					phases = append(phases, world.Phase{
						After: time.Duration(i) * 20 * time.Second, State: "working",
						Append: "still renaming...\n",
					})
				}
				return world.Script{Phases: phases}
			})
		},
		Checks: []Check{
			ResultSuccess(),
			SpawnCount(1),
			AnswerMatches("(?i)(still|running|not (yet )?finished|in progress|taking longer|hasn't|has not)"),
		},
		Notes: "The agent never finishes. The re-await discipline must bound the wait: end the turn honestly instead of stalling to the timeout.",
	}
}

func fastFinisherRelay() Scenario {
	return Scenario{
		ID:       "fast-finisher-relay",
		Category: "fault",
		Prompt:   "Spawn an agent in the docs worktree to fix the typo in the README installation section, wait for it, and confirm what it fixed — include the completion token it prints.",
		Timeout:  4 * time.Minute,
		Setup: func(w *world.World) {
			w.Worktrees = []world.Worktree{
				{ID: "wt-docs", Path: "/Users/bench/worktrees/docs", Branch: "docs"},
			}
			w.SetSpawnScript(func(agentID, worktreeID, prompt string) world.Script {
				return world.Script{Phases: []world.Phase{
					{After: 0, State: "working", Append: "Fixing typo...\n"},
					{After: 1500 * time.Millisecond, State: "waiting", WaitingReason: "prompt", Append: "Typo fixed: 'instalation' -> 'installation' in README.md. token=FA-901\n"},
				}}
			})
		},
		Checks: []Check{
			ResultSuccess(),
			SpawnCount(1),
			AnswerContains("FA-901"),
			Under(3 * time.Minute),
		},
		Notes: "The relay race: the agent finishes before the first status poll ever sees 'working' — the wait must still settle promptly.",
	}
}

func throttledReadsRecover() Scenario {
	return Scenario{
		ID:       "throttled-reads-recover",
		Category: "fault",
		Prompt:   "The nightly report agent in the report-runner terminal finished — what's the report ID it printed?",
		Timeout:  4 * time.Minute,
		Setup: func(w *world.World) {
			// Blank inline tails force the DEEP getOutput path, so the throttle
			// fault is guaranteed to be on the road to the answer.
			w.Faults.BlankStatusTail = true
			w.Faults.ThrottleGetOutputN = 2
			w.AddTerminal(world.Terminal{
				ID: "terminal-report-runner", Name: "Claude: report-runner", AgentID: "claude",
				SpawnedAt: backdated(),
				Script: world.Script{Phases: []world.Phase{
					{After: 0, State: "waiting", WaitingReason: "prompt", Append: "Nightly aggregation complete across 12 datasets.\nREPORT_ID=TH-2231\n"},
				}},
			})
		},
		Checks: []Check{
			ResultSuccess(),
			// 2 throttled attempts + at least 1 successful read.
			WorldCalled("terminal.getOutput", 3),
			AnswerContains("TH-2231"),
			SpawnCount(0),
		},
		Notes: "The first two deep reads are rate-limited (real MCP_RATE_LIMITED shape): the client-side retry must recover and still deliver the value.",
	}
}

func questionSurfacedNotHung() Scenario {
	return Scenario{
		ID:       "question-surfaced-not-hung",
		Category: "fault",
		Prompt:   "Spawn an agent in the data worktree to migrate the user tables to the new schema, wait for it, and report.",
		Timeout:  5 * time.Minute,
		Setup: func(w *world.World) {
			w.Worktrees = []world.Worktree{
				{ID: "wt-data", Path: "/Users/bench/worktrees/data", Branch: "data"},
			}
			w.SetSpawnScript(func(agentID, worktreeID, prompt string) world.Script {
				return world.Script{Phases: []world.Phase{
					{After: 0, State: "working", Append: "Migrating user tables...\n"},
					{After: 10 * time.Second, State: "waiting", WaitingReason: "question", Append: "User tables migrated. One thing: should I also migrate the legacy analytics tables? They're not in the ticket. (yes/no) QUESTION_ID=QA-8812\n"},
				}}
			})
		},
		Checks: []Check{
			ResultSuccess(),
			SpawnCount(1),
			AnswerMatches("(?i)(analytics|QA-8812)"),
			Under(4 * time.Minute),
		},
		Notes: "Mid-task question: the turn must surface the agent's question to the user, not hang or ignore it.",
	}
}

// --- latency: response-speed decomposition ------------------------------------
//
// These scenarios exist for their RoundDetail metrics, not their checks: cheap,
// fast, deterministic turns whose per-round latency decomposition (gap / raw meta /
// skill cue / committed meta / first token / cache hit) is the benchmark. Diff two
// runs' latency tables to see what a prompt/backend/CLI change did to response speed.

func latencyChat() Scenario {
	return Scenario{
		ID:       "latency-chat",
		Category: "latency",
		Prompt:   "Reply with the single word: pong",
		Timeout:  2 * time.Minute,
		Checks: []Check{
			ResultSuccess(),
			AnswerContains("pong"),
			SpawnCount(0),
		},
		Notes: "Pure conversational round-trip in an empty world: isolates first-response latency with no tool work.",
	}
}

func latencyToolRead() Scenario {
	return Scenario{
		ID:       "latency-tool-read",
		Category: "latency",
		Prompt:   "The build agent in the build-runner terminal just finished — what BUILD_TAG did it print? Answer with just the tag.",
		Timeout:  2 * time.Minute,
		Setup: func(w *world.World) {
			w.AddTerminal(world.Terminal{
				ID: "terminal-build-runner", Name: "Claude: build-runner", AgentID: "claude",
				SpawnedAt: backdated(),
				Script: world.Script{Phases: []world.Phase{
					{After: 0, State: "waiting", WaitingReason: "prompt", Append: "Build complete in 41s.\nBUILD_TAG=bt-5f21c9\nAll artifacts uploaded.\n"},
				}},
			})
		},
		Checks: []Check{
			ResultSuccess(),
			WorldCalledAny(1, "terminal.getOutput", "terminal.getStatus"),
			SpawnCount(0),
			AnswerContains("bt-5f21c9"),
		},
		Notes: "Canonical read-then-answer turn: measures inter-round gaps and prompt-cache reuse across rounds.",
	}
}
