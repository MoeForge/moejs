// Runs the Buffer cases (bufprobe.mjs) in Node.js and prints their
// results as JSON: node bufgen.mjs > buf_node.json
import { probeBuffer } from './bufprobe.mjs';
process.stdout.write(JSON.stringify(probeBuffer(Buffer), null, 1) + '\n');
