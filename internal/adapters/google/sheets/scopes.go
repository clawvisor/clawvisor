package sheets

const (
	// Full read/write access to Sheets spreadsheets.
	scopeSheets = "https://www.googleapis.com/auth/spreadsheets"
	// Scope used for listing spreadsheets via Drive (files.list).
	scopeDriveReadonly = "https://www.googleapis.com/auth/drive.readonly"
	// Default identity detection.
	scopeUserInfoEmail = "https://www.googleapis.com/auth/userinfo.email"
)

var sheetsScopes = []string{
	scopeSheets,
	scopeDriveReadonly,
	scopeUserInfoEmail,
}
