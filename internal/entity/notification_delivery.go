package entity

import "errors"

// NotificationDeliveryCanRetry reports whether the sender has evidence that
// the platform did not accept any part of the notification.
func NotificationDeliveryCanRetry(err error) bool {
	var unsent *UnsentError
	var rejection *DeliveryRejection
	return errors.As(err, &unsent) || errors.As(err, &rejection)
}
