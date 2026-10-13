import type { ReactNode } from 'react'

interface RowProps {
  /** The row's name, on the left. */
  label: ReactNode
  /** Ties the label to its input: a real <label for>, so the name is the control's. */
  htmlFor?: string
  /** For a control that is a group rather than an input (aria-labelledby). */
  labelId?: string
  /** The muted line under the label: a range, a gloss, a path. */
  sub?: ReactNode
  /** Path-like sublabels clip with an ellipsis instead of wrapping. */
  subTitle?: string
  /** Right-aligned. */
  control?: ReactNode
  /** Lines under the whole row (copy, env notes, errors): they span both columns. */
  children?: ReactNode
  /** 'stack' puts the control under the label, for rows whose control is wide. */
  layout?: 'side' | 'stack'
}

/**
 * One row of a settings card: label and muted sublabel on the left, the
 * control right-aligned, and any notes under the whole row. Every page of the
 * window is built from these, so a label, a range hint and an env note sit in
 * the same place on every page.
 */
export function Row({ label, htmlFor, labelId, sub, subTitle, control, children, layout = 'side' }: RowProps) {
  return (
    <div className={layout === 'stack' ? 'row is-stack' : 'row'}>
      <div className="lab">
        {htmlFor ? (
          <label className="t" htmlFor={htmlFor}>
            {label}
          </label>
        ) : (
          <span className="t" id={labelId}>
            {label}
          </span>
        )}
        {sub && (
          <span className={subTitle ? 's is-path' : 's'} title={subTitle}>
            {sub}
          </span>
        )}
      </div>
      {control !== undefined && <div className="ctl">{control}</div>}
      {children}
    </div>
  )
}
