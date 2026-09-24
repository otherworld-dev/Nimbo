package shellns

// NavNode is an Explorer navigation-pane (sidebar) folder entry in HKCU.
type NavNode struct {
	CLSID  string
	Target string // TargetFolderPath
	Icon   string // DefaultIcon
}
