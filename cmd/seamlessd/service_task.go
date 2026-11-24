package main

// The Windows service is an at-logon Scheduled Task, and Task Scheduler's root
// folder is the whole computer's. While every install registered \Seamless, a
// second user's install replaced the first user's task or was refused it, and
// a second user's uninstall deleted the first user's. So the task carries its
// user's SID, as the updater's own task does (updateTaskName):
// Seamless-<SID>.
//
// Releases before that registered \Seamless. install-service moves one of this
// user's to the per-user name (registerServiceTask). Until it has -- a pinned
// older release was reinstalled, or the old task is one only an admin may
// delete -- the verbs that act on the task find that one too, by its
// principal. Another user's \Seamless is never started, ended or deleted.
const (
	serviceTaskPrefix = "Seamless-"
	legacyServiceTask = "Seamless"
)

// serviceTaskName is the task of the user whose SID is sid.
func serviceTaskName(sid string) string { return serviceTaskPrefix + sid }

// taskLookup reads Scheduled Tasks for this user, injected so a test drives it
// on any OS.
type taskLookup struct {
	// sid is this user's SID, S-1-...
	sid string
	// queryXML reads the definition of the task called name (schtasks /Query
	// /TN name /XML); an error means none could be read: there is no such
	// task, or this user may not read it.
	queryXML func(name string) ([]byte, error)
	// isCurrentUser reports whether a task principal names this user.
	isCurrentUser func(account string) (bool, error)
}

// registeredTask is a Seamless task as this user can read it.
type registeredTask struct {
	name string
	def  taskDefinition
	// mine and foreign say whose its principal is: this user's, or another
	// account's. Neither holds when there is no task, it cannot be read, or
	// its principal cannot be matched to an account.
	mine, foreign bool
}

// read reads the task called name.
func (l taskLookup) read(name string) registeredTask {
	t := registeredTask{name: name}
	raw, err := l.queryXML(name)
	if err != nil {
		return t
	}
	def, err := parseTaskXML(raw)
	if err != nil || def.UserID == "" {
		return t
	}
	t.def = def
	mine, err := l.isCurrentUser(def.UserID)
	if err != nil {
		return t
	}
	t.mine, t.foreign = mine, !mine
	return t
}

// own lists this user's Seamless tasks: the per-user one, then a legacy
// \Seamless whose principal is this user. They are the tasks the service
// verbs may start and end and uninstall may delete; normally there is one.
func (l taskLookup) own() []registeredTask {
	var out []registeredTask
	for _, name := range []string{serviceTaskName(l.sid), legacyServiceTask} {
		if t := l.read(name); t.mine {
			out = append(out, t)
		}
	}
	return out
}

// taskNames is the tasks' names, in order.
func taskNames(tasks []registeredTask) []string {
	var names []string
	for _, t := range tasks {
		names = append(names, t.name)
	}
	return names
}

// findServiceTaskXML reads the task this user's daemon runs from, for the
// install probe: the per-user task, else the \Seamless a release before it
// registered. It does not judge the principal; gate 9 does
// (windowsSupervised). With neither readable it answers the per-user name and
// that read's error.
func findServiceTaskXML(sid string, query func(name string) ([]byte, error)) (string, []byte, error) {
	name := serviceTaskName(sid)
	raw, err := query(name)
	if err == nil {
		return name, raw, nil
	}
	if legacy, lerr := query(legacyServiceTask); lerr == nil {
		return legacyServiceTask, legacy, nil
	}
	return name, nil, err
}
