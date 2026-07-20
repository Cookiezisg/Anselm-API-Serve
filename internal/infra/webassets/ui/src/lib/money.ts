// Dashboard cost wires use whole micro-USD integers. Keep the conversion in one
// place so every cost view applies the same 1 USD = 1,000,000 micro-USD scale.
const MICRO_USD_PER_USD = 1_000_000

const usdFormatter = new Intl.NumberFormat('en-US', {
  style: 'currency',
  currency: 'USD',
  minimumFractionDigits: 2,
  maximumFractionDigits: 6,
})

export function formatMicroUsd(microUsd: number): string {
  return usdFormatter.format(microUsd / MICRO_USD_PER_USD)
}
