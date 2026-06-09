import SetupChecklist from '../components/SetupChecklist'
import GetStarted from './GetStarted'

// The Quickstart route is the canonical first-run destination. It surfaces
// the new SetupChecklist (the authoritative source of "what's left to do")
// on top of the existing GetStarted content, which still covers the
// step-by-step setup walkthrough until that content is folded in.
export default function Quickstart() {
  return (
    <div className="space-y-6">
      <div className="px-4 sm:px-8 pt-6">
        <SetupChecklist variant="full" />
      </div>
      <GetStarted />
    </div>
  )
}
