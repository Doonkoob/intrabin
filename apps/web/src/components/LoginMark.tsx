import { Activity } from 'lucide-react'

/** A finite CSS-only introduction: no renderer, frame loop or pointer tracking. */
export function LoginMark() {
  return (
    <div className="login-mark" aria-hidden="true" data-testid="login-mark">
      <div className="login-mark-scene">
        <div className="login-mark-plate login-mark-base"><i /><i /><i /></div>
        <div className="login-mark-plate login-mark-middle"><i /><i /><i /></div>
        <div className="login-mark-plate login-mark-top"><Activity /></div>
      </div>
    </div>
  )
}
