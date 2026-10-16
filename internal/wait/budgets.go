package wait

import "time"

// The budgets, one per kind of wait. Each errs long: running one out is a
// loud failure the operator must act on, while a condition that holds early
// costs nothing. A test shortens a copy, never these.
var (
	// HubReady: a cold hub start after a reboot (daemon re-exec, cold page
	// cache, a large hub.db) can take many seconds before it serves at all.
	HubReady = Spec{
		Name: "the hub to answer", Budget: 2 * time.Minute, Interval: time.Second,
		NoticeAfter: 15 * time.Second, NoticeEvery: 30 * time.Second,
	}
	// HubStartRetryPause is the pause before the one restart of a hub that
	// exited during start-up.
	HubStartRetryPause = 3 * time.Second

	// RuntimeBrokerOnline: the runtime broker registers with the hub only
	// after the hub serves.
	RuntimeBrokerOnline = Spec{
		Name: "the hub's runtime broker to come online", Budget: 30 * time.Second,
		MinAttempts: 30, Interval: time.Second,
	}

	// BrokerUnavailable absorbs the same registration race on a scion verb
	// that answers "no runtime broker".
	BrokerUnavailable = Attempts("a runtime broker for the scion verb", 30, time.Second)

	// ManagerPhaseSettle: a record in a transitional phase, until scion
	// settles it.
	ManagerPhaseSettle = Attempts("the manager's phase to settle", 60, time.Second)

	// ManagerRunning: a started or resumed record, until the hub reads it
	// running over a live container. ManagerSettle is how long it must then
	// stay so when no harness signal decides sooner.
	ManagerRunning = Attempts("the manager to run", 15, time.Second)
	ManagerSettle  = 10 * time.Second

	// ManagerStopped: a suspend or stop, until the hub reads the phase with
	// no live container.
	ManagerStopped = Attempts("the manager to stop", 30, time.Second)

	// ManagerMountsVerified: a resumed manager with no liveness gate, until
	// lever can read its tree mounts back.
	ManagerMountsVerified = Attempts("the resumed manager's mounts to verify", 30, time.Second)

	// SessionHealed is short: sciontool's hub call is synchronous, so the
	// first list normally shows the phase the heal reported.
	SessionHealed = Attempts("the healed session to read running", 3, time.Second)

	// StopClaimReleased: the hub releases a stop claim on its reaper's next
	// tick (every minute) after a broker heartbeat shows the container gone.
	StopClaimReleased = Spec{
		Name: "the hub to release its stop claim", MinAttempts: 9,
		Interval: 5 * time.Second, MaxInterval: 30 * time.Second,
	}
)
