package jobs

// A subagent's frames captured from OpenCode 2.3.0 by spike S-SA1,
// verbatim but for the probe's working directory, which is written as
// /workspace: its prompt is a user item in its own inbox, delivered, and
// its turn ends as a root's does.
//
// @decision(DL-145) @decision(DL-114)
const (
	capturedRoot  = "ses_e1426d4d5ffedqhswKTyvXXqlM"
	capturedChild = "ses_e1426d269ffegGmE50lyaTg06K"

	capturedChildEnqueued = `{"id":"evt_1ebd92d9b001wX6P5hFr5SDZ2J","created":1794958241179,"type":"session.inbox.enqueued"` +
		`,"location":{"directory":"/workspace"},"data":{"inboxID":"msg_1ebd92d9a002W48UblNgHuEL7S"` +
		`,"sessionID":"ses_e1426d269ffegGmE50lyaTg06K","item":{"type":"user"` +
		`,"payload":{"text":"You are a subagent spawned by another session.\nCHILD-FG run one glob"}` +
		`,"delivery":"steer"}},"durable":{"aggregateID":"ses_e1426d269ffegGmE50lyaTg06K","seq":1` +
		`,"version":1}}`
	capturedChildDelivered = `{"id":"evt_1ebd92db8001B8nlOAaINtS1fC","created":1794958241209,"type":"session.inbox.delivered"` +
		`,"location":{"directory":"/workspace"},"data":{"sessionID":"ses_e1426d269ffegGmE50lyaTg06K"` +
		`,"inboxID":"msg_1ebd92d9a002W48UblNgHuEL7S"}` +
		`,"durable":{"aggregateID":"ses_e1426d269ffegGmE50lyaTg06K","seq":4,"version":1}}`
	capturedChildSucceeded = `{"id":"evt_1ebd930ea001GjfgUQbdrfPNN5","created":1794958242026,"type":"session.execution.succeeded"` +
		`,"data":{"sessionID":"ses_e1426d269ffegGmE50lyaTg06K"}` +
		`,"durable":{"aggregateID":"ses_e1426d269ffegGmE50lyaTg06K","seq":19,"version":1}}`
)
