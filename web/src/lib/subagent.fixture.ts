import type { Activity, Conversation } from "./timeline";

/**
 * A session whose agent ran one subagent, as the API serves it: the
 * conversations and log of OpenCode 2.2.5's measured S-SA1 frames (the
 * foreground pair), read through the real recorder into a store. Only the
 * fake model's words are rewritten; every shape is the measured one. The
 * child is listed first, as the store lists it.
 *
 * @decision(DL-111) @decision(DL-110)
 */
export const Root = "ses_e436799f4ffenGTEhkUrSrHIcl";
export const Child = "ses_e43679656ffezjEh709cjEIOFv";

/** The child's task prompt: the parent agent's words, stored as `user`. */
export const ChildPrompt = "You are a subagent spawned by another session.\nReview the diff of this branch against main, focusing on the retry change in internal/client/retry.go. Report any bug you find and list the files you read.";

export const subagentConversations: Conversation[] = [
  {
    "id": "ses_e43679656ffezjEh709cjEIOFv",
    "title": "Review the retry change",
    "parent": "ses_e436799f4ffenGTEhkUrSrHIcl",
    "messages": [
      {
        "id": "msg_1bc9869b20018Wf1Z1H8kMNdk7",
        "role": "user",
        "text": "You are a subagent spawned by another session.\nReview the diff of this branch against main, focusing on the retry change in internal/client/retry.go. Report any bug you find and list the files you read.",
        "at": "2026-11-08T19:17:47.584Z"
      },
      {
        "id": "msg_1bc986f00001eFbAUzpySFsib4",
        "role": "assistant",
        "text": "The retry change is safe to merge. Backoff is capped at 30 s and the context is checked before every attempt. I read internal/client/retry.go and retry_test.go.",
        "at": "2026-11-08T19:17:48.938Z"
      }
    ]
  },
  {
    "id": "ses_e436799f4ffenGTEhkUrSrHIcl",
    "title": "title",
    "messages": [
      {
        "id": "msg_01a1bc98661aspike0001",
        "role": "user",
        "text": "Can you check that the retry change on this branch is safe to merge? Have a subagent review the diff.",
        "at": "2026-11-08T19:17:47.489Z"
      },
      {
        "id": "msg_1bc986f5c001FoX60SRi7A0qbK",
        "role": "assistant",
        "text": "The subagent found no bugs in the retry change. Backoff is capped at 30 s and the context is checked before every attempt, so it is safe to merge.",
        "at": "2026-11-08T19:17:49.025Z"
      }
    ]
  }
];

export const subagentLog: Activity[] = [
  {
    "name": "session.model.selected",
    "session": "ses_e436799f4ffenGTEhkUrSrHIcl",
    "source": "opencode",
    "model": "probe/m",
    "at": "2026-11-08T19:17:46.648Z",
    "pos": 1
  },
  {
    "name": "session.inbox.enqueued",
    "session": "ses_e436799f4ffenGTEhkUrSrHIcl",
    "source": "opencode",
    "role": "user",
    "message": "msg_01a1bc98661aspike0001",
    "delivery": "steer",
    "text": "Can you check that the retry change on this branch is safe to merge? Have a subagent review the diff.",
    "at": "2026-11-08T19:17:47.468Z",
    "pos": 2
  },
  {
    "name": "session.execution.started",
    "session": "ses_e436799f4ffenGTEhkUrSrHIcl",
    "source": "opencode",
    "at": "2026-11-08T19:17:47.473Z",
    "pos": 3
  },
  {
    "name": "session.instructions.updated",
    "session": "ses_e436799f4ffenGTEhkUrSrHIcl",
    "source": "opencode",
    "at": "2026-11-08T19:17:47.487Z",
    "pos": 4
  },
  {
    "name": "session.inbox.delivered",
    "session": "ses_e436799f4ffenGTEhkUrSrHIcl",
    "source": "opencode",
    "message": "msg_01a1bc98661aspike0001",
    "at": "2026-11-08T19:17:47.489Z",
    "pos": 5
  },
  {
    "name": "session.step.started",
    "session": "ses_e436799f4ffenGTEhkUrSrHIcl",
    "source": "opencode",
    "at": "2026-11-08T19:17:47.532Z",
    "pos": 6
  },
  {
    "name": "session.tool.input.started",
    "session": "ses_e436799f4ffenGTEhkUrSrHIcl",
    "source": "opencode",
    "call": "call_1794165467525768415",
    "tool": "subagent",
    "at": "2026-11-08T19:17:47.536Z",
    "pos": 7
  },
  {
    "name": "session.tool.input.ended",
    "session": "ses_e436799f4ffenGTEhkUrSrHIcl",
    "source": "opencode",
    "text": "{\"agent\":\"general\",\"description\":\"Review the retry change\",\"prompt\":\"Review the diff of this branch against main, focusing on the retry change in internal/client/retry.go. Report any bug you find and list the files you read.\"}",
    "call": "call_1794165467525768415",
    "at": "2026-11-08T19:17:47.542Z",
    "pos": 8
  },
  {
    "name": "session.tool.called",
    "session": "ses_e436799f4ffenGTEhkUrSrHIcl",
    "source": "opencode",
    "call": "call_1794165467525768415",
    "at": "2026-11-08T19:17:47.545Z",
    "pos": 9
  },
  {
    "name": "session.renamed",
    "session": "ses_e436799f4ffenGTEhkUrSrHIcl",
    "source": "opencode",
    "text": "title",
    "at": "2026-11-08T19:17:47.556Z",
    "pos": 10
  },
  {
    "name": "session.inbox.enqueued",
    "session": "ses_e43679656ffezjEh709cjEIOFv",
    "source": "opencode",
    "role": "user",
    "message": "msg_1bc9869b20018Wf1Z1H8kMNdk7",
    "delivery": "steer",
    "text": "You are a subagent spawned by another session.\nReview the diff of this branch against main, focusing on the retry change in internal/client/retry.go. Report any bug you find and list the files you read.",
    "at": "2026-11-08T19:17:47.57Z",
    "pos": 11
  },
  {
    "name": "session.execution.started",
    "session": "ses_e43679656ffezjEh709cjEIOFv",
    "source": "opencode",
    "at": "2026-11-08T19:17:47.573Z",
    "pos": 12
  },
  {
    "name": "session.instructions.updated",
    "session": "ses_e43679656ffezjEh709cjEIOFv",
    "source": "opencode",
    "at": "2026-11-08T19:17:47.579Z",
    "pos": 13
  },
  {
    "name": "session.inbox.delivered",
    "session": "ses_e43679656ffezjEh709cjEIOFv",
    "source": "opencode",
    "message": "msg_1bc9869b20018Wf1Z1H8kMNdk7",
    "at": "2026-11-08T19:17:47.584Z",
    "pos": 14
  },
  {
    "name": "session.step.started",
    "session": "ses_e43679656ffezjEh709cjEIOFv",
    "source": "opencode",
    "at": "2026-11-08T19:17:47.603Z",
    "pos": 15
  },
  {
    "name": "session.tool.input.started",
    "session": "ses_e43679656ffezjEh709cjEIOFv",
    "source": "opencode",
    "call": "call_1794165467598799599",
    "tool": "glob",
    "at": "2026-11-08T19:17:47.607Z",
    "pos": 16
  },
  {
    "name": "session.tool.input.ended",
    "session": "ses_e43679656ffezjEh709cjEIOFv",
    "source": "opencode",
    "text": "{\"pattern\":\"internal/client/*.go\"}",
    "call": "call_1794165467598799599",
    "at": "2026-11-08T19:17:47.61Z",
    "pos": 17
  },
  {
    "name": "session.tool.called",
    "session": "ses_e43679656ffezjEh709cjEIOFv",
    "source": "opencode",
    "call": "call_1794165467598799599",
    "at": "2026-11-08T19:17:47.613Z",
    "pos": 18
  },
  {
    "name": "session.tool.success",
    "session": "ses_e43679656ffezjEh709cjEIOFv",
    "source": "opencode",
    "call": "call_1794165467598799599",
    "at": "2026-11-08T19:17:48.908Z",
    "pos": 19
  },
  {
    "name": "session.step.ended",
    "session": "ses_e43679656ffezjEh709cjEIOFv",
    "source": "opencode",
    "at": "2026-11-08T19:17:48.915Z",
    "pos": 20
  },
  {
    "name": "session.step.started",
    "session": "ses_e43679656ffezjEh709cjEIOFv",
    "source": "opencode",
    "at": "2026-11-08T19:17:48.945Z",
    "pos": 21
  },
  {
    "name": "session.text.started",
    "session": "ses_e43679656ffezjEh709cjEIOFv",
    "source": "opencode",
    "at": "2026-11-08T19:17:48.951Z",
    "pos": 22
  },
  {
    "name": "session.text.ended",
    "session": "ses_e43679656ffezjEh709cjEIOFv",
    "source": "opencode",
    "text": "The retry change is safe to merge. Backoff is capped at 30 s and the context is checked before every attempt. I read internal/client/retry.go and retry_test.go.",
    "at": "2026-11-08T19:17:48.97Z",
    "pos": 23
  },
  {
    "name": "session.step.ended",
    "session": "ses_e43679656ffezjEh709cjEIOFv",
    "source": "opencode",
    "at": "2026-11-08T19:17:48.981Z",
    "pos": 24
  },
  {
    "name": "session.execution.succeeded",
    "session": "ses_e43679656ffezjEh709cjEIOFv",
    "source": "opencode",
    "at": "2026-11-08T19:17:48.99Z",
    "pos": 25
  },
  {
    "name": "session.tool.success",
    "session": "ses_e436799f4ffenGTEhkUrSrHIcl",
    "source": "opencode",
    "call": "call_1794165467525768415",
    "at": "2026-11-08T19:17:49Z",
    "pos": 26
  },
  {
    "name": "session.step.ended",
    "session": "ses_e436799f4ffenGTEhkUrSrHIcl",
    "source": "opencode",
    "at": "2026-11-08T19:17:49.006Z",
    "pos": 27
  },
  {
    "name": "session.step.started",
    "session": "ses_e436799f4ffenGTEhkUrSrHIcl",
    "source": "opencode",
    "at": "2026-11-08T19:17:49.031Z",
    "pos": 28
  },
  {
    "name": "session.text.started",
    "session": "ses_e436799f4ffenGTEhkUrSrHIcl",
    "source": "opencode",
    "at": "2026-11-08T19:17:49.035Z",
    "pos": 29
  },
  {
    "name": "session.text.ended",
    "session": "ses_e436799f4ffenGTEhkUrSrHIcl",
    "source": "opencode",
    "text": "The subagent found no bugs in the retry change. Backoff is capped at 30 s and the context is checked before every attempt, so it is safe to merge.",
    "at": "2026-11-08T19:17:49.039Z",
    "pos": 30
  },
  {
    "name": "session.step.ended",
    "session": "ses_e436799f4ffenGTEhkUrSrHIcl",
    "source": "opencode",
    "at": "2026-11-08T19:17:49.044Z",
    "pos": 31
  },
  {
    "name": "session.execution.succeeded",
    "session": "ses_e436799f4ffenGTEhkUrSrHIcl",
    "source": "opencode",
    "at": "2026-11-08T19:17:49.046Z",
    "pos": 32
  }
];
