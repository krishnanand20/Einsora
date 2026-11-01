//go:build !darwin && !linux

package process

import "time"

func Start(Spec) (Proc, error) { return Proc{}, ErrUnsupported }

func Alive(Proc) bool { return false }

func Tree(...Proc) []Proc { return nil }

func Stop(Proc, time.Duration) bool { return true }

func StopAll([]Proc, time.Duration) bool { return true }
