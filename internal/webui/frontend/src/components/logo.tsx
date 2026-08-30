/**
 * The product mark: a descending stack (the queue) beside a download arrow.
 *
 * Drawn in `currentColor` on purpose — `.brand .logo` colours it with
 * `var(--accent)`, so it follows the theme switch for free and needs no
 * light/dark variants. The favicon in public/favicon.svg is the same geometry
 * on a filled badge, which is what reads at 16px against a browser tab.
 */
export function Logo({ size = 22 }: { size?: number }) {
  return (
    <svg
      class="logo"
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      role="img"
      aria-label="yt-dlp-manager"
    >
      <path
        d="M3 6.5h8.5M3 12h6.5M3 17.5h4.5"
        stroke="currentColor"
        stroke-width="2.5"
        stroke-linecap="round"
      />
      <path d="M17.6 5.6v6.2" stroke="currentColor" stroke-width="3" stroke-linecap="round" />
      <path d="M13.3 10.5h8.6L17.6 18z" fill="currentColor" />
    </svg>
  )
}
