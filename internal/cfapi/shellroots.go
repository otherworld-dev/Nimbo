package cfapi

// ShellSyncRoot is a sync root registered with Explorer under Nimbo's
// provider name, as the registry records it.
type ShellSyncRoot struct {
	ID             string // "Nimbo!<user SID>!<hash>"
	Path           string // the folder, from UserSyncRoots
	Icon           string // IconResource: the executable that registered it, ",0"
	NamespaceCLSID string // the Explorer sidebar entry Windows made for it
}
