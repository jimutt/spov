package server

// Frames captured from OpenCode 2.3.0 by spike S-SA1 (a root conversation
// and its subagent), verbatim but for the probe's working directory, which
// is written as /workspace.
//
// @decision(DL-145) @decision(DL-110)
const (
	capturedRoot  = "ses_e1426d4d5ffedqhswKTyvXXqlM"
	capturedChild = "ses_e1426d269ffegGmE50lyaTg06K"

	capturedRootCreated = `{"id":"evt_1ebd92b2e001j85oPgXukh4WdY","created":1794958240558,"type":"session.created"` +
		`,"location":{"directory":"/workspace"},"data":{"sessionID":"ses_e1426d4d5ffedqhswKTyvXXqlM"` +
		`,"slug":"lucky-circuit","version":"2.3.0","projectID":"ea8cfb83de64ba31fc67c2d078b17991068d887f"` +
		`,"location":{"directory":"/workspace"},"subpath":""}` +
		`,"durable":{"aggregateID":"ses_e1426d4d5ffedqhswKTyvXXqlM","seq":0,"version":1}}`
	capturedRootStepStarted = `{"id":"evt_1ebd92d83001BiT1a3Z4s1vAXc","created":1794958241155,"type":"session.step.started"` +
		`,"location":{"directory":"/workspace"},"data":{"sessionID":"ses_e1426d4d5ffedqhswKTyvXXqlM"` +
		`,"agent":"build","model":{"id":"m","providerID":"probe","variant":"default"}` +
		`,"assistantMessageID":"msg_1ebd92d6c0010uh8rr7znJasC7","started":1794958241137}` +
		`,"durable":{"aggregateID":"ses_e1426d4d5ffedqhswKTyvXXqlM","seq":6,"version":1}}`
	capturedChildCreated = `{"id":"evt_1ebd92d98001LfQtU7bGCsjd5h","created":1794958241176,"type":"session.created"` +
		`,"location":{"directory":"/workspace"},"data":{"sessionID":"ses_e1426d269ffegGmE50lyaTg06K"` +
		`,"slug":"kind-lagoon","version":"2.3.0","projectID":"ea8cfb83de64ba31fc67c2d078b17991068d887f"` +
		`,"parentID":"ses_e1426d4d5ffedqhswKTyvXXqlM","location":{"directory":"/workspace"},"subpath":""` +
		`,"title":"probe child fg","agent":"general","model":{"id":"m","providerID":"probe"` +
		`,"variant":"default"}},"durable":{"aggregateID":"ses_e1426d269ffegGmE50lyaTg06K","seq":0` +
		`,"version":1}}`
	capturedChildEnqueued = `{"id":"evt_1ebd92d9b001wX6P5hFr5SDZ2J","created":1794958241179,"type":"session.inbox.enqueued"` +
		`,"location":{"directory":"/workspace"},"data":{"inboxID":"msg_1ebd92d9a002W48UblNgHuEL7S"` +
		`,"sessionID":"ses_e1426d269ffegGmE50lyaTg06K","item":{"type":"user"` +
		`,"payload":{"text":"You are a subagent spawned by another session.\nCHILD-FG run one glob"}` +
		`,"delivery":"steer"}},"durable":{"aggregateID":"ses_e1426d269ffegGmE50lyaTg06K","seq":1` +
		`,"version":1}}`
	capturedChildStarted = `{"id":"evt_1ebd92db3001kgI3LWrihluNuH","created":1794958241203,"type":"session.execution.started"` +
		`,"data":{"sessionID":"ses_e1426d269ffegGmE50lyaTg06K"}` +
		`,"durable":{"aggregateID":"ses_e1426d269ffegGmE50lyaTg06K","seq":2,"version":1}}`
	capturedChildDelivered = `{"id":"evt_1ebd92db8001B8nlOAaINtS1fC","created":1794958241209,"type":"session.inbox.delivered"` +
		`,"location":{"directory":"/workspace"},"data":{"sessionID":"ses_e1426d269ffegGmE50lyaTg06K"` +
		`,"inboxID":"msg_1ebd92d9a002W48UblNgHuEL7S"}` +
		`,"durable":{"aggregateID":"ses_e1426d269ffegGmE50lyaTg06K","seq":4,"version":1}}`
	capturedChildSucceeded = `{"id":"evt_1ebd930ea001GjfgUQbdrfPNN5","created":1794958242026,"type":"session.execution.succeeded"` +
		`,"data":{"sessionID":"ses_e1426d269ffegGmE50lyaTg06K"}` +
		`,"durable":{"aggregateID":"ses_e1426d269ffegGmE50lyaTg06K","seq":19,"version":1}}`
)
