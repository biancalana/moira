package notifier

import (
	"fmt"
	"github.com/moira-alert/moira-alert"
)

type NotificationPackage struct {
	Events     []moira.EventData
	Trigger    moira.TriggerData
	Contact    moira.ContactData
	FailCount  int
	Throttled  bool
	DontResend bool
}

func (pkg NotificationPackage) String() string {
	return fmt.Sprintf("package of %d notifications to %s", len(pkg.Events), pkg.Contact.Value)
}
