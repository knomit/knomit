package repos

// credentialChanged is what every credential change ends with: a stored
// secret's value replaced, a link or unlink, or a pasted-credential swap. It
// asks the origin loop to close its circuit breakers (the one scoped
// exception in breaker.go) and wakes it, so the new credential is tried at
// once instead of after an open period or the next tick. Nil-safe.
func (ri *RepoInstance) credentialChanged() {
	if ri == nil {
		return
	}
	ri.breakers.requestReset()
	ri.wakeSync()
}
