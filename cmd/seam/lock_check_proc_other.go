//go:build !darwin && !linux

package main

import "errors"

// errLcNoProcessTable is an OS this check reads no process table on.
var errLcNoProcessTable = errors.New("no process table reader on this OS")

func lcProcLookup(int) (int, string, error) { return 0, "", errLcNoProcessTable }

func lcProcArgs(int) ([]string, error) { return nil, errLcNoProcessTable }
