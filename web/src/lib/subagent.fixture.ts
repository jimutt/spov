import type { Activity, Conversation } from "./timeline";

/**
 * Sessions whose agent ran a subagent, as the API serves them: the
 * conversations and log of spike S-SA1's measured frames, read through
 * the real recorder into a store (`TestWriteTheSubagentFixture` in
 * internal/server). Only the fake model's words are rewritten; every
 * shape is the measured one. The child is listed after its parent, as
 * the store lists it at 2.3.0.
 *
 * The first is OpenCode 2.3.0's, where every subagent runs in the
 * background: the parent's call returns at once naming the child, the
 * parent's turn ends while the child works, and the child's report
 * reaches the parent as a synthetic inbox item that starts its next
 * turn. The other two were recorded at 2.2.5 and are kept so a session
 * recorded then still draws: its foreground pair, whose call returned
 * when the child was done, and its background pair, whose call was
 * made with `background: true`.
 *
 * @decision(DL-111) @decision(DL-110) @decision(DL-112) @decision(DL-144)
 */
export const Root = "ses_e1471e21effe4tvd6dRsbSyJeb";
export const Child = "ses_e1471df72ffecZP6FsFzTsQbBa";
/** The root's call to the subagent tool. */
export const Call = "call_1794953322603787133";

/** The child's task prompt: the parent agent's words, stored as `user`. */
export const ChildPrompt = "You are a subagent spawned by another session.\nReview the diff of this branch against main, focusing on the retry change in internal/client/retry.go. Report any bug you find and list the files you read.";

export const subagentConversations: Conversation[] = [
  {
    "id": "ses_e1471e21effe4tvd6dRsbSyJeb",
    "title": "title",
    "messages": [
      {
        "id": "msg_1eb8e1dfc001u8yXFg0MzlHp7f",
        "role": "model-switched",
        "at": "2026-11-17T22:08:41.98Z"
      },
      {
        "id": "msg_01a1eb8e1dfespike0001",
        "role": "user",
        "text": "Can you check that the retry change on this branch is safe to merge? Have a subagent review the diff.",
        "at": "2026-11-17T22:08:42.567Z"
      },
      {
        "id": "msg_1eb8e2061001sDA5c1rmUfzO6z",
        "role": "assistant",
        "at": "2026-11-17T22:08:42.6Z"
      },
      {
        "id": "msg_1eb8e20a4001AnlOLVd2Av7Oab",
        "role": "assistant",
        "text": "A subagent is reviewing the retry change in the background. I will report back when it finishes.",
        "at": "2026-11-17T22:08:42.665Z"
      },
      {
        "id": "msg_1eb8e22df00116mRGNlx3Hf3jG",
        "role": "idle",
        "at": "2026-11-17T22:08:43.231Z"
      },
      {
        "id": "msg_1eb8e2096001rsTFIhAbeAfYj3",
        "role": "synthetic",
        "text": "<subagent sessionID=\"ses_e1471df72ffecZP6FsFzTsQbBa\" state=\"completed\" description=\"Review the retry change\">\nThe retry change is safe to merge. Backoff is capped at 30 s and the context is checked before every attempt. I read internal/client/retry.go and retry_test.go.\n</subagent>",
        "at": "2026-11-17T22:08:45.005Z"
      },
      {
        "id": "msg_1eb8e29d0001DuiVLW7Gdka7gs",
        "role": "assistant",
        "text": "The subagent found no bugs in the retry change. Backoff is capped at 30 s and the context is checked before every attempt, so it is safe to merge.",
        "at": "2026-11-17T22:08:45.012Z"
      },
      {
        "id": "msg_1eb8e29e70011IT313oJwOonvl",
        "role": "idle",
        "at": "2026-11-17T22:08:45.031Z"
      }
    ]
  },
  {
    "id": "ses_e1471df72ffecZP6FsFzTsQbBa",
    "title": "Review the retry change",
    "parent": "ses_e1471e21effe4tvd6dRsbSyJeb",
    "messages": [
      {
        "id": "msg_1eb8e20940012UW255x7ax5jin",
        "role": "user",
        "text": "You are a subagent spawned by another session.\nReview the diff of this branch against main, focusing on the retry change in internal/client/retry.go. Report any bug you find and list the files you read.",
        "at": "2026-11-17T22:08:43.187Z"
      },
      {
        "id": "msg_1eb8e22ba001LH32KvWnXYxZOV",
        "role": "assistant",
        "at": "2026-11-17T22:08:43.201Z"
      },
      {
        "id": "msg_1eb8e29a90017Q3MX8dEwUximP",
        "role": "assistant",
        "text": "The retry change is safe to merge. Backoff is capped at 30 s and the context is checked before every attempt. I read internal/client/retry.go and retry_test.go.",
        "at": "2026-11-17T22:08:44.974Z"
      },
      {
        "id": "msg_1eb8e29c30012uQlQ3g8TVV0wr",
        "role": "idle",
        "at": "2026-11-17T22:08:44.995Z"
      }
    ]
  }
];

export const subagentLog: Activity[] = [
  {
    "name": "session.model.selected",
    "session": "ses_e1471e21effe4tvd6dRsbSyJeb",
    "source": "opencode",
    "model": "probe/m",
    "at": "2026-11-17T22:08:41.98Z",
    "pos": 1
  },
  {
    "name": "session.inbox.enqueued",
    "session": "ses_e1471e21effe4tvd6dRsbSyJeb",
    "source": "opencode",
    "role": "user",
    "message": "msg_01a1eb8e1dfespike0001",
    "delivery": "steer",
    "text": "Can you check that the retry change on this branch is safe to merge? Have a subagent review the diff.",
    "at": "2026-11-17T22:08:42.549Z",
    "pos": 2
  },
  {
    "name": "session.execution.started",
    "session": "ses_e1471e21effe4tvd6dRsbSyJeb",
    "source": "opencode",
    "at": "2026-11-17T22:08:42.553Z",
    "pos": 3
  },
  {
    "name": "session.instructions.updated",
    "session": "ses_e1471e21effe4tvd6dRsbSyJeb",
    "source": "opencode",
    "at": "2026-11-17T22:08:42.564Z",
    "pos": 4
  },
  {
    "name": "session.inbox.delivered",
    "session": "ses_e1471e21effe4tvd6dRsbSyJeb",
    "source": "opencode",
    "message": "msg_01a1eb8e1dfespike0001",
    "at": "2026-11-17T22:08:42.567Z",
    "pos": 5
  },
  {
    "name": "session.step.started",
    "session": "ses_e1471e21effe4tvd6dRsbSyJeb",
    "source": "opencode",
    "at": "2026-11-17T22:08:42.611Z",
    "pos": 6
  },
  {
    "name": "session.step.first-token",
    "session": "ses_e1471e21effe4tvd6dRsbSyJeb",
    "source": "opencode",
    "at": "2026-11-17T22:08:42.614Z",
    "pos": 7
  },
  {
    "name": "session.tool.input.started",
    "session": "ses_e1471e21effe4tvd6dRsbSyJeb",
    "source": "opencode",
    "call": "call_1794953322603787133",
    "tool": "subagent",
    "at": "2026-11-17T22:08:42.619Z",
    "pos": 8
  },
  {
    "name": "session.tool.input.ended",
    "session": "ses_e1471e21effe4tvd6dRsbSyJeb",
    "source": "opencode",
    "text": "{\"agent\":\"general\",\"description\":\"Review the retry change\",\"prompt\":\"Review the diff of this branch against main, focusing on the retry change in internal/client/retry.go. Report any bug you find and list the files you read.\"}",
    "call": "call_1794953322603787133",
    "at": "2026-11-17T22:08:42.624Z",
    "pos": 9
  },
  {
    "name": "session.tool.called",
    "session": "ses_e1471e21effe4tvd6dRsbSyJeb",
    "source": "opencode",
    "call": "call_1794953322603787133",
    "at": "2026-11-17T22:08:42.626Z",
    "pos": 10
  },
  {
    "name": "session.renamed",
    "session": "ses_e1471e21effe4tvd6dRsbSyJeb",
    "source": "opencode",
    "text": "title",
    "at": "2026-11-17T22:08:42.634Z",
    "pos": 11
  },
  {
    "name": "session.inbox.enqueued",
    "session": "ses_e1471df72ffecZP6FsFzTsQbBa",
    "source": "opencode",
    "role": "user",
    "message": "msg_1eb8e20940012UW255x7ax5jin",
    "delivery": "steer",
    "text": "You are a subagent spawned by another session.\nReview the diff of this branch against main, focusing on the retry change in internal/client/retry.go. Report any bug you find and list the files you read.",
    "at": "2026-11-17T22:08:42.645Z",
    "pos": 12
  },
  {
    "name": "session.tool.success",
    "session": "ses_e1471e21effe4tvd6dRsbSyJeb",
    "source": "opencode",
    "call": "call_1794953322603787133",
    "child": "ses_e1471df72ffecZP6FsFzTsQbBa",
    "at": "2026-11-17T22:08:42.647Z",
    "pos": 13
  },
  {
    "name": "session.step.ended",
    "session": "ses_e1471e21effe4tvd6dRsbSyJeb",
    "source": "opencode",
    "at": "2026-11-17T22:08:42.651Z",
    "pos": 14
  },
  {
    "name": "session.execution.started",
    "session": "ses_e1471df72ffecZP6FsFzTsQbBa",
    "source": "opencode",
    "at": "2026-11-17T22:08:43.179Z",
    "pos": 15
  },
  {
    "name": "session.instructions.updated",
    "session": "ses_e1471df72ffecZP6FsFzTsQbBa",
    "source": "opencode",
    "at": "2026-11-17T22:08:43.185Z",
    "pos": 16
  },
  {
    "name": "session.inbox.delivered",
    "session": "ses_e1471df72ffecZP6FsFzTsQbBa",
    "source": "opencode",
    "message": "msg_1eb8e20940012UW255x7ax5jin",
    "at": "2026-11-17T22:08:43.187Z",
    "pos": 17
  },
  {
    "name": "session.step.started",
    "session": "ses_e1471e21effe4tvd6dRsbSyJeb",
    "source": "opencode",
    "at": "2026-11-17T22:08:43.218Z",
    "pos": 18
  },
  {
    "name": "session.step.first-token",
    "session": "ses_e1471e21effe4tvd6dRsbSyJeb",
    "source": "opencode",
    "at": "2026-11-17T22:08:43.22Z",
    "pos": 19
  },
  {
    "name": "session.text.started",
    "session": "ses_e1471e21effe4tvd6dRsbSyJeb",
    "source": "opencode",
    "at": "2026-11-17T22:08:43.221Z",
    "pos": 20
  },
  {
    "name": "session.text.ended",
    "session": "ses_e1471e21effe4tvd6dRsbSyJeb",
    "source": "opencode",
    "text": "A subagent is reviewing the retry change in the background. I will report back when it finishes.",
    "at": "2026-11-17T22:08:43.224Z",
    "pos": 21
  },
  {
    "name": "session.step.ended",
    "session": "ses_e1471e21effe4tvd6dRsbSyJeb",
    "source": "opencode",
    "at": "2026-11-17T22:08:43.226Z",
    "pos": 22
  },
  {
    "name": "session.execution.succeeded",
    "session": "ses_e1471e21effe4tvd6dRsbSyJeb",
    "source": "opencode",
    "at": "2026-11-17T22:08:43.231Z",
    "pos": 23
  },
  {
    "name": "session.step.started",
    "session": "ses_e1471df72ffecZP6FsFzTsQbBa",
    "source": "opencode",
    "at": "2026-11-17T22:08:43.233Z",
    "pos": 24
  },
  {
    "name": "session.step.first-token",
    "session": "ses_e1471df72ffecZP6FsFzTsQbBa",
    "source": "opencode",
    "at": "2026-11-17T22:08:43.235Z",
    "pos": 25
  },
  {
    "name": "session.tool.input.started",
    "session": "ses_e1471df72ffecZP6FsFzTsQbBa",
    "source": "opencode",
    "call": "call_1794953323204335765",
    "tool": "glob",
    "at": "2026-11-17T22:08:43.237Z",
    "pos": 26
  },
  {
    "name": "session.tool.input.ended",
    "session": "ses_e1471df72ffecZP6FsFzTsQbBa",
    "source": "opencode",
    "text": "{\"pattern\":\"internal/client/*.go\"}",
    "call": "call_1794953323204335765",
    "at": "2026-11-17T22:08:43.24Z",
    "pos": 27
  },
  {
    "name": "session.tool.called",
    "session": "ses_e1471df72ffecZP6FsFzTsQbBa",
    "source": "opencode",
    "call": "call_1794953323204335765",
    "at": "2026-11-17T22:08:43.241Z",
    "pos": 28
  },
  {
    "name": "session.tool.success",
    "session": "ses_e1471df72ffecZP6FsFzTsQbBa",
    "source": "opencode",
    "call": "call_1794953323204335765",
    "at": "2026-11-17T22:08:44.958Z",
    "pos": 29
  },
  {
    "name": "session.step.ended",
    "session": "ses_e1471df72ffecZP6FsFzTsQbBa",
    "source": "opencode",
    "at": "2026-11-17T22:08:44.961Z",
    "pos": 30
  },
  {
    "name": "session.step.started",
    "session": "ses_e1471df72ffecZP6FsFzTsQbBa",
    "source": "opencode",
    "at": "2026-11-17T22:08:44.98Z",
    "pos": 31
  },
  {
    "name": "session.step.first-token",
    "session": "ses_e1471df72ffecZP6FsFzTsQbBa",
    "source": "opencode",
    "at": "2026-11-17T22:08:44.982Z",
    "pos": 32
  },
  {
    "name": "session.text.started",
    "session": "ses_e1471df72ffecZP6FsFzTsQbBa",
    "source": "opencode",
    "at": "2026-11-17T22:08:44.984Z",
    "pos": 33
  },
  {
    "name": "session.text.ended",
    "session": "ses_e1471df72ffecZP6FsFzTsQbBa",
    "source": "opencode",
    "text": "The retry change is safe to merge. Backoff is capped at 30 s and the context is checked before every attempt. I read internal/client/retry.go and retry_test.go.",
    "at": "2026-11-17T22:08:44.987Z",
    "pos": 34
  },
  {
    "name": "session.step.ended",
    "session": "ses_e1471df72ffecZP6FsFzTsQbBa",
    "source": "opencode",
    "at": "2026-11-17T22:08:44.992Z",
    "pos": 35
  },
  {
    "name": "session.execution.succeeded",
    "session": "ses_e1471df72ffecZP6FsFzTsQbBa",
    "source": "opencode",
    "at": "2026-11-17T22:08:44.995Z",
    "pos": 36
  },
  {
    "name": "session.inbox.enqueued",
    "session": "ses_e1471e21effe4tvd6dRsbSyJeb",
    "source": "opencode",
    "role": "synthetic",
    "message": "msg_1eb8e2096001rsTFIhAbeAfYj3",
    "text": "<subagent sessionID=\"ses_e1471df72ffecZP6FsFzTsQbBa\" state=\"completed\" description=\"Review the retry change\">\nThe retry change is safe to merge. Backoff is capped at 30 s and the context is checked before every attempt. I read internal/client/retry.go and retry_test.go.\n</subagent>",
    "at": "2026-11-17T22:08:45Z",
    "pos": 37
  },
  {
    "name": "session.execution.started",
    "session": "ses_e1471e21effe4tvd6dRsbSyJeb",
    "source": "opencode",
    "at": "2026-11-17T22:08:45.002Z",
    "pos": 38
  },
  {
    "name": "session.inbox.delivered",
    "session": "ses_e1471e21effe4tvd6dRsbSyJeb",
    "source": "opencode",
    "message": "msg_1eb8e2096001rsTFIhAbeAfYj3",
    "at": "2026-11-17T22:08:45.005Z",
    "pos": 39
  },
  {
    "name": "session.step.started",
    "session": "ses_e1471e21effe4tvd6dRsbSyJeb",
    "source": "opencode",
    "at": "2026-11-17T22:08:45.017Z",
    "pos": 40
  },
  {
    "name": "session.step.first-token",
    "session": "ses_e1471e21effe4tvd6dRsbSyJeb",
    "source": "opencode",
    "at": "2026-11-17T22:08:45.02Z",
    "pos": 41
  },
  {
    "name": "session.text.started",
    "session": "ses_e1471e21effe4tvd6dRsbSyJeb",
    "source": "opencode",
    "at": "2026-11-17T22:08:45.022Z",
    "pos": 42
  },
  {
    "name": "session.text.ended",
    "session": "ses_e1471e21effe4tvd6dRsbSyJeb",
    "source": "opencode",
    "text": "The subagent found no bugs in the retry change. Backoff is capped at 30 s and the context is checked before every attempt, so it is safe to merge.",
    "at": "2026-11-17T22:08:45.025Z",
    "pos": 43
  },
  {
    "name": "session.step.ended",
    "session": "ses_e1471e21effe4tvd6dRsbSyJeb",
    "source": "opencode",
    "at": "2026-11-17T22:08:45.028Z",
    "pos": 44
  },
  {
    "name": "session.execution.succeeded",
    "session": "ses_e1471e21effe4tvd6dRsbSyJeb",
    "source": "opencode",
    "at": "2026-11-17T22:08:45.031Z",
    "pos": 45
  }
];

/** The 2.2.5 foreground pair: the parent's call returned when its child was done, and its result named the child. */
export const ForegroundRoot = "ses_e436799f4ffenGTEhkUrSrHIcl";
export const ForegroundChild = "ses_e43679656ffezjEh709cjEIOFv";

export const foregroundConversations: Conversation[] = [
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

export const foregroundLog: Activity[] = [
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
    "child": "ses_e43679656ffezjEh709cjEIOFv",
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

/** The 2.2.5 background pair: a call made with `background: true`, whose child outlived it. */
export const BackgroundRoot = "ses_e43678fbfffefjBXuBacJ2DjbK";
export const BackgroundChild = "ses_e43678f5cffeokSb2gKuNL7Wq4";

export const backgroundConversations: Conversation[] = [
  {
    "id": "ses_e43678f5cffeokSb2gKuNL7Wq4",
    "title": "Audit the docs for stale commands",
    "parent": "ses_e43678fbfffefjBXuBacJ2DjbK",
    "messages": [
      {
        "id": "msg_1bc9870aa001GMd6C7MDzwS6nf",
        "role": "user",
        "text": "You are a subagent spawned by another session.\nSearch docs/ for commands that no longer exist in the CLI, and list each file and line.",
        "at": "2026-11-08T19:17:49.387Z"
      },
      {
        "id": "msg_1bc987bdf001OuAzCS9mx3oEfb",
        "role": "assistant",
        "text": "No stale commands found in docs/. I checked 14 files against the current command tree.",
        "at": "2026-11-08T19:17:52.226Z"
      }
    ]
  },
  {
    "id": "ses_e43678fbfffefjBXuBacJ2DjbK",
    "title": "title",
    "messages": [
      {
        "id": "msg_01a1bc987061spike0002",
        "role": "user",
        "text": "While I finish the changelog, have a subagent audit the docs for stale commands in the background.",
        "at": "2026-11-08T19:17:49.295Z"
      },
      {
        "id": "msg_1bc9870bb001WmslZxSSGvNrQ6",
        "role": "assistant",
        "text": "The docs audit is running in the background. I will pick up its result when it finishes.",
        "at": "2026-11-08T19:17:49.376Z"
      },
      {
        "id": "msg_1bc9870ac001ybNybCo3aqs5Mj",
        "role": "synthetic",
        "text": "<subagent sessionID=\"ses_e43678f5cffeokSb2gKuNL7Wq4\" state=\"completed\" description=\"Audit the docs for stale commands\">\nNo stale commands found in docs/. I checked 14 files against the current command tree.\n</subagent>",
        "at": "2026-11-08T19:17:52.245Z"
      },
      {
        "id": "msg_1bc987bf9001iqmeGQb7AGPaXm",
        "role": "assistant",
        "text": "The docs audit is running in the background. I will pick up its result when it finishes.",
        "at": "2026-11-08T19:17:52.253Z"
      }
    ]
  }
];

export const backgroundLog: Activity[] = [
  {
    "name": "session.model.selected",
    "session": "ses_e43678fbfffefjBXuBacJ2DjbK",
    "source": "opencode",
    "model": "probe/m",
    "at": "2026-11-08T19:17:49.278Z",
    "pos": 1
  },
  {
    "name": "session.inbox.enqueued",
    "session": "ses_e43678fbfffefjBXuBacJ2DjbK",
    "source": "opencode",
    "role": "user",
    "message": "msg_01a1bc987061spike0002",
    "delivery": "steer",
    "text": "While I finish the changelog, have a subagent audit the docs for stale commands in the background.",
    "at": "2026-11-08T19:17:49.285Z",
    "pos": 2
  },
  {
    "name": "session.execution.started",
    "session": "ses_e43678fbfffefjBXuBacJ2DjbK",
    "source": "opencode",
    "at": "2026-11-08T19:17:49.287Z",
    "pos": 3
  },
  {
    "name": "session.instructions.updated",
    "session": "ses_e43678fbfffefjBXuBacJ2DjbK",
    "source": "opencode",
    "at": "2026-11-08T19:17:49.291Z",
    "pos": 4
  },
  {
    "name": "session.inbox.delivered",
    "session": "ses_e43678fbfffefjBXuBacJ2DjbK",
    "source": "opencode",
    "message": "msg_01a1bc987061spike0002",
    "at": "2026-11-08T19:17:49.295Z",
    "pos": 5
  },
  {
    "name": "session.renamed",
    "session": "ses_e43678fbfffefjBXuBacJ2DjbK",
    "source": "opencode",
    "text": "title",
    "at": "2026-11-08T19:17:49.328Z",
    "pos": 6
  },
  {
    "name": "session.step.started",
    "session": "ses_e43678fbfffefjBXuBacJ2DjbK",
    "source": "opencode",
    "at": "2026-11-08T19:17:49.334Z",
    "pos": 7
  },
  {
    "name": "session.tool.input.started",
    "session": "ses_e43678fbfffefjBXuBacJ2DjbK",
    "source": "opencode",
    "call": "call_1794165469323048833",
    "tool": "subagent",
    "at": "2026-11-08T19:17:49.336Z",
    "pos": 8
  },
  {
    "name": "session.tool.input.ended",
    "session": "ses_e43678fbfffefjBXuBacJ2DjbK",
    "source": "opencode",
    "text": "{\"agent\":\"general\",\"description\":\"Audit the docs for stale commands\",\"prompt\":\"Search docs/ for commands that no longer exist in the CLI, and list each file and line.\",\"background\":true}",
    "call": "call_1794165469323048833",
    "at": "2026-11-08T19:17:49.339Z",
    "pos": 9
  },
  {
    "name": "session.tool.called",
    "session": "ses_e43678fbfffefjBXuBacJ2DjbK",
    "source": "opencode",
    "call": "call_1794165469323048833",
    "at": "2026-11-08T19:17:49.341Z",
    "pos": 10
  },
  {
    "name": "session.inbox.enqueued",
    "session": "ses_e43678f5cffeokSb2gKuNL7Wq4",
    "source": "opencode",
    "role": "user",
    "message": "msg_1bc9870aa001GMd6C7MDzwS6nf",
    "delivery": "steer",
    "text": "You are a subagent spawned by another session.\nSearch docs/ for commands that no longer exist in the CLI, and list each file and line.",
    "at": "2026-11-08T19:17:49.354Z",
    "pos": 11
  },
  {
    "name": "session.tool.success",
    "session": "ses_e43678fbfffefjBXuBacJ2DjbK",
    "source": "opencode",
    "call": "call_1794165469323048833",
    "child": "ses_e43678f5cffeokSb2gKuNL7Wq4",
    "at": "2026-11-08T19:17:49.357Z",
    "pos": 12
  },
  {
    "name": "session.step.ended",
    "session": "ses_e43678fbfffefjBXuBacJ2DjbK",
    "source": "opencode",
    "at": "2026-11-08T19:17:49.359Z",
    "pos": 13
  },
  {
    "name": "session.execution.started",
    "session": "ses_e43678f5cffeokSb2gKuNL7Wq4",
    "source": "opencode",
    "at": "2026-11-08T19:17:49.379Z",
    "pos": 14
  },
  {
    "name": "session.instructions.updated",
    "session": "ses_e43678f5cffeokSb2gKuNL7Wq4",
    "source": "opencode",
    "at": "2026-11-08T19:17:49.385Z",
    "pos": 15
  },
  {
    "name": "session.inbox.delivered",
    "session": "ses_e43678f5cffeokSb2gKuNL7Wq4",
    "source": "opencode",
    "message": "msg_1bc9870aa001GMd6C7MDzwS6nf",
    "at": "2026-11-08T19:17:49.387Z",
    "pos": 16
  },
  {
    "name": "session.step.started",
    "session": "ses_e43678fbfffefjBXuBacJ2DjbK",
    "source": "opencode",
    "at": "2026-11-08T19:17:49.402Z",
    "pos": 17
  },
  {
    "name": "session.text.started",
    "session": "ses_e43678fbfffefjBXuBacJ2DjbK",
    "source": "opencode",
    "at": "2026-11-08T19:17:49.404Z",
    "pos": 18
  },
  {
    "name": "session.text.ended",
    "session": "ses_e43678fbfffefjBXuBacJ2DjbK",
    "source": "opencode",
    "text": "The docs audit is running in the background. I will pick up its result when it finishes.",
    "at": "2026-11-08T19:17:49.407Z",
    "pos": 19
  },
  {
    "name": "session.step.ended",
    "session": "ses_e43678fbfffefjBXuBacJ2DjbK",
    "source": "opencode",
    "at": "2026-11-08T19:17:49.409Z",
    "pos": 20
  },
  {
    "name": "session.execution.succeeded",
    "session": "ses_e43678fbfffefjBXuBacJ2DjbK",
    "source": "opencode",
    "at": "2026-11-08T19:17:49.411Z",
    "pos": 21
  },
  {
    "name": "session.step.started",
    "session": "ses_e43678f5cffeokSb2gKuNL7Wq4",
    "source": "opencode",
    "at": "2026-11-08T19:17:52.192Z",
    "pos": 22
  },
  {
    "name": "session.tool.input.started",
    "session": "ses_e43678f5cffeokSb2gKuNL7Wq4",
    "source": "opencode",
    "call": "call_1794165472185186233",
    "tool": "glob",
    "at": "2026-11-08T19:17:52.201Z",
    "pos": 23
  },
  {
    "name": "session.tool.input.ended",
    "session": "ses_e43678f5cffeokSb2gKuNL7Wq4",
    "source": "opencode",
    "text": "{\"pattern\":\"docs/**/*.md\"}",
    "call": "call_1794165472185186233",
    "at": "2026-11-08T19:17:52.204Z",
    "pos": 24
  },
  {
    "name": "session.tool.called",
    "session": "ses_e43678f5cffeokSb2gKuNL7Wq4",
    "source": "opencode",
    "call": "call_1794165472185186233",
    "at": "2026-11-08T19:17:52.206Z",
    "pos": 25
  },
  {
    "name": "session.tool.success",
    "session": "ses_e43678f5cffeokSb2gKuNL7Wq4",
    "source": "opencode",
    "call": "call_1794165472185186233",
    "at": "2026-11-08T19:17:52.216Z",
    "pos": 26
  },
  {
    "name": "session.step.ended",
    "session": "ses_e43678f5cffeokSb2gKuNL7Wq4",
    "source": "opencode",
    "at": "2026-11-08T19:17:52.218Z",
    "pos": 27
  },
  {
    "name": "session.step.started",
    "session": "ses_e43678f5cffeokSb2gKuNL7Wq4",
    "source": "opencode",
    "at": "2026-11-08T19:17:52.229Z",
    "pos": 28
  },
  {
    "name": "session.text.started",
    "session": "ses_e43678f5cffeokSb2gKuNL7Wq4",
    "source": "opencode",
    "at": "2026-11-08T19:17:52.231Z",
    "pos": 29
  },
  {
    "name": "session.text.ended",
    "session": "ses_e43678f5cffeokSb2gKuNL7Wq4",
    "source": "opencode",
    "text": "No stale commands found in docs/. I checked 14 files against the current command tree.",
    "at": "2026-11-08T19:17:52.233Z",
    "pos": 30
  },
  {
    "name": "session.step.ended",
    "session": "ses_e43678f5cffeokSb2gKuNL7Wq4",
    "source": "opencode",
    "at": "2026-11-08T19:17:52.235Z",
    "pos": 31
  },
  {
    "name": "session.execution.succeeded",
    "session": "ses_e43678f5cffeokSb2gKuNL7Wq4",
    "source": "opencode",
    "at": "2026-11-08T19:17:52.237Z",
    "pos": 32
  },
  {
    "name": "session.inbox.enqueued",
    "session": "ses_e43678fbfffefjBXuBacJ2DjbK",
    "source": "opencode",
    "role": "synthetic",
    "message": "msg_1bc9870ac001ybNybCo3aqs5Mj",
    "text": "<subagent sessionID=\"ses_e43678f5cffeokSb2gKuNL7Wq4\" state=\"completed\" description=\"Audit the docs for stale commands\">\nNo stale commands found in docs/. I checked 14 files against the current command tree.\n</subagent>",
    "at": "2026-11-08T19:17:52.239Z",
    "pos": 33
  },
  {
    "name": "session.execution.started",
    "session": "ses_e43678fbfffefjBXuBacJ2DjbK",
    "source": "opencode",
    "at": "2026-11-08T19:17:52.24Z",
    "pos": 34
  },
  {
    "name": "session.inbox.delivered",
    "session": "ses_e43678fbfffefjBXuBacJ2DjbK",
    "source": "opencode",
    "message": "msg_1bc9870ac001ybNybCo3aqs5Mj",
    "at": "2026-11-08T19:17:52.245Z",
    "pos": 35
  },
  {
    "name": "session.step.started",
    "session": "ses_e43678fbfffefjBXuBacJ2DjbK",
    "source": "opencode",
    "at": "2026-11-08T19:17:52.257Z",
    "pos": 36
  },
  {
    "name": "session.text.started",
    "session": "ses_e43678fbfffefjBXuBacJ2DjbK",
    "source": "opencode",
    "at": "2026-11-08T19:17:52.258Z",
    "pos": 37
  },
  {
    "name": "session.text.ended",
    "session": "ses_e43678fbfffefjBXuBacJ2DjbK",
    "source": "opencode",
    "text": "The docs audit is running in the background. I will pick up its result when it finishes.",
    "at": "2026-11-08T19:17:52.26Z",
    "pos": 38
  },
  {
    "name": "session.step.ended",
    "session": "ses_e43678fbfffefjBXuBacJ2DjbK",
    "source": "opencode",
    "at": "2026-11-08T19:17:52.262Z",
    "pos": 39
  },
  {
    "name": "session.execution.succeeded",
    "session": "ses_e43678fbfffefjBXuBacJ2DjbK",
    "source": "opencode",
    "at": "2026-11-08T19:17:52.263Z",
    "pos": 40
  }
];
