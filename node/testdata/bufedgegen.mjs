// node bufedgegen.mjs > bufedge_node.json
import * as nb from 'node:buffer';
import { edgeBuffer } from './bufedge.mjs';
process.stdout.write(JSON.stringify(edgeBuffer(nb.Buffer, nb, { skipPanics: !!process.env.SKIP_PANICS }), null, 1) + '\n');
