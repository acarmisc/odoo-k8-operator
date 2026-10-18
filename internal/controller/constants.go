package controller

// Shared names used by both controllers; kept in one place so labels, selectors
// and volume names can't drift apart.
const (
	labelApp      = "app"
	labelInstance = "instance"
	appOdoo       = "odoo"

	volumeAddons = "addons"
	volumeData   = "data"
	volumeConfig = "odoo-config"
)
