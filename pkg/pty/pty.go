package pty

import (
	"os"
	"os/exec"

	creackpty "github.com/creack/pty"
)

type Process struct {
	File *os.File
	Cmd  *exec.Cmd
}

func Start(cmd *exec.Cmd, cols, rows int) (*Process, error) {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}

	file, err := creackpty.StartWithSize(cmd, &creackpty.Winsize{
		Cols: uint16(cols),
		Rows: uint16(rows),
	})
	if err != nil {
		return nil, err
	}

	return &Process{File: file, Cmd: cmd}, nil
}

func Resize(file *os.File, cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}

	return creackpty.Setsize(file, &creackpty.Winsize{
		Cols: uint16(cols),
		Rows: uint16(rows),
	})
}
