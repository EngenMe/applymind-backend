// Package applications is the module boundary for job applications and their
// directly-owned records: application_status_history (audit trail of status
// transitions) and recruiter_contacts (optional PII-isolated contact info).
// Row types live in the generated sqlc package (internal/db/sqlc); this file
// is a placeholder for module-specific view/request types once handlers gain
// logic.
package applications
