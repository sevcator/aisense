// Representative literal declarations from OmniRoute a3ca33fa6442b59adc42976c795709eaf5351109.
export const AUTO_TEMPLATE_VARIANTS: Record<string, AutoVariant | undefined> = {
  "auto/best-coding": "coding",
  "auto/best-reasoning": "smart",
  "auto/best-fast": "fast",
  "auto/best-vision": "smart",
  "auto/best-chat": undefined,
  // Chaos mode
  "auto/chaos": "chaos",
};
export const AUTO_SUFFIX_VARIANTS: string[] = [
  "auto/coding:fast",
  "auto/reasoning",
];
// Functions outside these declarations are not interpreted.
export function ignored() { throw new Error("never execute source"); }
