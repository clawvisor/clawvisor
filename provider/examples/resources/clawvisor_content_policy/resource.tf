# A content-scanning rule. "block" rules stop the request with block_message;
# "flag" rules accumulate the rule name for observation.
# Requires the local_governance capability (spec 06a) on OSS.
resource "clawvisor_content_policy" "block_ssn" {
  name          = "block-ssn"
  pattern       = "\\d{3}-\\d{2}-\\d{4}"
  pattern_kind  = "regex"
  action        = "block"
  block_message = "SSN-shaped content is not allowed."
}
