
interface IconProps {
  size?: number
}

const base = (size?: number) => ({
  width: size ?? 18,
  height: size ?? 18,
  viewBox: '0 0 24 24',
  fill: 'none',
  stroke: 'currentColor',
  'stroke-width': 2,
  'stroke-linecap': 'round' as const,
  'stroke-linejoin': 'round' as const,
  'aria-hidden': true as const
})

export const IconQueue = ({ size }: IconProps) => (
  <svg {...base(size)}>
    <path d="M4 6h16M4 12h16M4 18h10" />
  </svg>
)

export const IconLibrary = ({ size }: IconProps) => (
  <svg {...base(size)}>
    <path d="M4 19V5a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v14" />
    <path d="M17 8h1a2 2 0 0 1 2 2v11H6" />
  </svg>
)

export const IconSettings = ({ size }: IconProps) => (
  <svg {...base(size)}>
    <circle cx="12" cy="12" r="3" />
    <path d="M19.4 15a1.7 1.7 0 0 0 .3 1.9l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.7 1.7 0 0 0-1.9-.3 1.7 1.7 0 0 0-1 1.5V21a2 2 0 1 1-4 0v-.1a1.7 1.7 0 0 0-1-1.6 1.7 1.7 0 0 0-1.9.3l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1a1.7 1.7 0 0 0 .3-1.9 1.7 1.7 0 0 0-1.5-1H3a2 2 0 1 1 0-4h.1a1.7 1.7 0 0 0 1.6-1 1.7 1.7 0 0 0-.3-1.9l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1a1.7 1.7 0 0 0 1.9.3h.1a1.7 1.7 0 0 0 1-1.5V3a2 2 0 1 1 4 0v.1a1.7 1.7 0 0 0 1 1.5 1.7 1.7 0 0 0 1.9-.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.7 1.7 0 0 0-.3 1.9v.1a1.7 1.7 0 0 0 1.5 1H21a2 2 0 1 1 0 4h-.1a1.7 1.7 0 0 0-1.5 1z" />
  </svg>
)

export const IconDownload = ({ size }: IconProps) => (
  <svg {...base(size)}>
    <path d="M12 3v12m0 0 4-4m-4 4-4-4" />
    <path d="M4 21h16" />
  </svg>
)

export const IconPause = ({ size }: IconProps) => (
  <svg {...base(size)}>
    <path d="M9 5v14M15 5v14" />
  </svg>
)

export const IconPlay = ({ size }: IconProps) => (
  <svg {...base(size)}>
    <path d="M7 4.5v15l13-7.5z" />
  </svg>
)

export const IconTrash = ({ size }: IconProps) => (
  <svg {...base(size)}>
    <path d="M4 7h16M10 11v6M14 11v6" />
    <path d="M6 7l1 13h10l1-13M9 7V4h6v3" />
  </svg>
)

export const IconClose = ({ size }: IconProps) => (
  <svg {...base(size)}>
    <path d="M6 6l12 12M18 6L6 18" />
  </svg>
)

export const IconBolt = ({ size }: IconProps) => (
  <svg {...base(size)}>
    <path d="M13 2 4 14h6l-1 8 9-12h-6z" />
  </svg>
)

export const IconRetry = ({ size }: IconProps) => (
  <svg {...base(size)}>
    <path d="M20 12a8 8 0 1 1-2.34-5.66" />
    <path d="M20 3v5h-5" />
  </svg>
)
