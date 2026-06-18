package model

// TGSendResult is the outcome of posting/forwarding a document.
type TGSendResult struct {
	MessageID    int64
	FileID       string
	FileUniqueID string
}
