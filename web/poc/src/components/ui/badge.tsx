import * as React from 'react'
import { cva, type VariantProps } from 'class-variance-authority'
import { cn } from '@/lib/utils'

const badgeVariants = cva(
  'inline-flex items-center rounded-full border px-2.5 py-0.5 text-xs font-semibold transition-colors focus:outline-none focus:ring-2 focus:ring-slate-950 focus:ring-offset-2',
  {
    variants: {
      variant: {
        default:
          'border-transparent bg-slate-900 text-slate-50 hover:bg-slate-900/80',
        secondary:
          'border-transparent bg-slate-100 text-slate-900 hover:bg-slate-100/80',
        destructive:
          'border-transparent bg-red-500 text-slate-50 hover:bg-red-500/80',
        outline: 'text-slate-950',
        // Soft variants: dark text on light background — all pass WCAG AA (≥4.5:1).
        // green-800 on green-100 ≈ 7.2:1; yellow-800 on yellow-100 ≈ 8.1:1;
        // blue-800 on blue-100 ≈ 6.3:1.
        success:
          'border border-green-200 bg-green-100 text-green-800 hover:bg-green-100/80',
        warning:
          'border border-yellow-200 bg-yellow-100 text-yellow-800 hover:bg-yellow-100/80',
        info:
          'border border-blue-200 bg-blue-100 text-blue-800 hover:bg-blue-100/80',
      },
    },
    defaultVariants: {
      variant: 'default',
    },
  }
)

export interface BadgeProps
  extends React.HTMLAttributes<HTMLDivElement>,
    VariantProps<typeof badgeVariants> {}

function Badge({ className, variant, ...props }: BadgeProps) {
  return (
    <div className={cn(badgeVariants({ variant }), className)} {...props} />
  )
}

export { Badge, badgeVariants }
