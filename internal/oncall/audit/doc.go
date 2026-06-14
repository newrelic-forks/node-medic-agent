// Package audit owns the on-call UI's two observability surfaces: the
// FIFO ring-buffer annotation appended to NodeHealthDiagnosisAI CRs
// (FR-18a, N=20) and the structured stdout JSON log line emitted per
// action (FR-18). Real code lands in Phase 5 (T059, T060).
package audit
