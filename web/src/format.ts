export const nf = (value: number | null | undefined, digits = 0) => (value ?? 0).toLocaleString('da-DK', { maximumFractionDigits: digits })
export const sat = (msat: number) => `${nf(msat / 1000)} sat`
export const kr = (msat: number, rate: number) => `${nf(msat / 1e11 * rate, 2)} kr`
export function ago(time: number | null) { if (!time) return 'never'; const s = Date.now()/1000-time; return s < 120 ? 'just now' : s < 7200 ? `${Math.round(s/60)} min ago` : s < 172800 ? `${Math.round(s/3600)} h ago` : `${Math.round(s/86400)} d ago` }
