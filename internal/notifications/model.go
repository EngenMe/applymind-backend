// Package notifications is the module boundary for follow-up reminders (the
// "follow_up_reminders" table), swept and sent by the applymind-scheduler
// Lambda on its daily EventBridge cron. Row types live in the generated sqlc
// package (internal/db/sqlc); this file is a placeholder for module-specific
// view/request types once handlers gain logic.
package notifications
