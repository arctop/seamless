package main

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeTaskLookup reads the tasks in tasks, by name, as this user (fakeSID).
func fakeTaskLookup(tasks map[string][]byte) taskLookup {
	return taskLookup{
		sid: fakeSID,
		queryXML: func(name string) ([]byte, error) {
			if raw, ok := tasks[name]; ok {
				return raw, nil
			}
			return nil, errors.New("exit status 1")
		},
		isCurrentUser: func(account string) (bool, error) {
			if account == "NOBODY" {
				return false, errors.New("no mapping")
			}
			return account == fakeSID, nil
		},
	}
}

func TestServiceTaskName(t *testing.T) {
	require.Equal(t, "Seamless-S-1-5-21-1-2-3-1001", serviceTaskName("S-1-5-21-1-2-3-1001"))
	require.NotEqual(t, updateTaskName(fakeSID), serviceTaskName(fakeSID), "the updater's task is another task")
}

// TestTaskLookupOwn: the verbs that start, end and delete the task act only on
// this user's -- the per-user one, and an earlier release's \Seamless when its
// principal is this user -- never on another user's.
func TestTaskLookupOwn(t *testing.T) {
	const exe, args = `C:\s.exe`, "serve"
	mine := serviceTaskXML(fakeSID, exe, args)
	theirs := serviceTaskXML("S-1-5-21-1-2-3-1002", exe, args)
	perUser := "Seamless-" + fakeSID
	tests := []struct {
		name  string
		tasks map[string][]byte
		want  []string
	}{
		{"none", nil, nil},
		{"the per-user task", map[string][]byte{perUser: mine}, []string{perUser}},
		{"an earlier release's, this user's", map[string][]byte{"Seamless": mine}, []string{"Seamless"}},
		{"both, the per-user one first", map[string][]byte{"Seamless": mine, perUser: mine}, []string{perUser, "Seamless"}},
		{"another user's Seamless", map[string][]byte{"Seamless": theirs}, nil},
		{"another user's Seamless beside this user's task", map[string][]byte{"Seamless": theirs, perUser: mine}, []string{perUser}},
		{"a per-user name with another principal", map[string][]byte{perUser: theirs}, nil},
		{"a principal that cannot be told", map[string][]byte{"Seamless": serviceTaskXML("NOBODY", exe, args)}, nil},
		{"a definition that does not parse", map[string][]byte{perUser: []byte("not xml")}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, taskNames(fakeTaskLookup(tt.tasks).own()))
		})
	}
}

func TestTaskLookupRead(t *testing.T) {
	l := fakeTaskLookup(map[string][]byte{
		"Seamless":            serviceTaskXML("S-1-5-21-1-2-3-1002", `C:\s.exe`, "serve"),
		"Seamless-" + fakeSID: serviceTaskXML(fakeSID, `"C:\s.exe"`, "serve"),
	})
	theirs := l.read("Seamless")
	require.True(t, theirs.foreign)
	require.False(t, theirs.mine)
	require.Equal(t, "S-1-5-21-1-2-3-1002", theirs.def.UserID)

	mine := l.read("Seamless-" + fakeSID)
	require.True(t, mine.mine)
	require.Equal(t, `C:\s.exe`, mine.def.Command, "parseTaskXML trims the quotes")

	none := l.read("SeamlessUpdate-" + fakeSID)
	require.Equal(t, registeredTask{name: "SeamlessUpdate-" + fakeSID}, none, "absent: neither mine nor foreign")
}

// TestFindServiceTaskXML: the install probe reads the per-user task, else an
// earlier release's \Seamless, whoever's it is (gate 9 judges the principal).
func TestFindServiceTaskXML(t *testing.T) {
	perUser := "Seamless-" + fakeSID
	query := func(tasks map[string]string) func(string) ([]byte, error) {
		return func(name string) ([]byte, error) {
			if raw, ok := tasks[name]; ok {
				return []byte(raw), nil
			}
			return nil, errors.New("exit status 1")
		}
	}
	name, raw, err := findServiceTaskXML(fakeSID, query(map[string]string{perUser: "new", "Seamless": "old"}))
	require.NoError(t, err)
	require.Equal(t, perUser, name)
	require.Equal(t, "new", string(raw))

	name, raw, err = findServiceTaskXML(fakeSID, query(map[string]string{"Seamless": "old"}))
	require.NoError(t, err)
	require.Equal(t, "Seamless", name)
	require.Equal(t, "old", string(raw))

	name, _, err = findServiceTaskXML(fakeSID, query(nil))
	require.Error(t, err)
	require.Equal(t, perUser, name, "none: named as the task an install registers")
}
