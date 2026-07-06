# A per-window spend cap. One cap per window (daily/monthly).
# Requires the local_governance capability (spec 06a) on OSS.
resource "clawvisor_spend_cap" "daily" {
  window      = "daily"
  cap_micros  = 5000000 # $5.00
  enforcement = "soft"  # warn at 80%/100%; "hard" blocks at 100%
}
