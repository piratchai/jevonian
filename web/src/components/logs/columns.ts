/**
 * The Logs table grid template, shared by the header, rows, and skeleton so the
 * three never drift apart.
 *
 * Every track is `minmax(0, <fr>)`: the columns always sum to the container
 * width and can shrink to zero, so a narrow viewport clips cell content instead
 * of letting columns overlap. Tracks are sized by relative weight, with Model
 * and Provider getting the most room.
 */
export const LOG_COLUMNS =
  "minmax(0,1.1fr) minmax(0,1.6fr) minmax(0,1.3fr) minmax(0,0.9fr) minmax(0,0.9fr) minmax(0,0.8fr) minmax(0,0.9fr) minmax(0,1fr) minmax(0,1fr) minmax(0,1.2fr)";
