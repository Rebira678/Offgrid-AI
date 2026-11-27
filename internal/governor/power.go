package governor

import (
	"os"
	"path/filepath"
	"strings"
)

// Power says whether the machine is running on its battery.
type Power interface {
	// OnBattery is true only when a battery is discharging and no mains supply is online. A desktop,
	// a Pi and anything whose power state cannot be read count as "not on battery": the check must not
	// be what stops a node from ever working.
	OnBattery() bool
}

// Sysfs reads /sys/class/power_supply (Linux, which covers the laptop, WSL2 excepted, and the Pi).
type Sysfs struct{ Root string } // "" means /sys/class/power_supply

// OnBattery implements Power.
func (s Sysfs) OnBattery() bool {
	root := s.Root
	if root == "" {
		root = "/sys/class/power_supply"
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	discharging := false
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		kind := strings.TrimSpace(readFile(filepath.Join(dir, "type")))
		switch strings.ToLower(kind) {
		case "mains", "usb", "usb_c", "usb_pd", "usb_dcp", "usb_cdp", "wireless":
			if strings.TrimSpace(readFile(filepath.Join(dir, "online"))) == "1" {
				return false // a charger is plugged in
			}
		case "battery":
			if strings.EqualFold(strings.TrimSpace(readFile(filepath.Join(dir, "status"))), "discharging") {
				discharging = true
			}
		}
	}
	return discharging
}

func readFile(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}

// Always and Never are fixed power states, for tests and for a flag.
type fixedPower bool

func (f fixedPower) OnBattery() bool { return bool(f) }

// FixedPower returns a Power that always answers b.
func FixedPower(b bool) Power { return fixedPower(b) }
