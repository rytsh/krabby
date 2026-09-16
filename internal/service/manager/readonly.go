package manager

// SetReadOnly sets the process capability before configuring clients or serving
// requests. It must not be changed on a running manager.
func (m *Manager) SetReadOnly(enabled bool) { m.readOnly = enabled }

// ReadOnly is also safe during MCP catalog inspection without a manager.
func (m *Manager) ReadOnly() bool { return m != nil && m.readOnly }
