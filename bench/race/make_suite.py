#!/usr/bin/env python3
"""Assembles the V8 benchmark suite v7 sources, unchanged, with one shared
driver into the programs every engine runs:

- fixed.js: the relay. One fixed-work iteration runs each component of a
  benchmark (Crypto: Encrypt then Decrypt; EarleyBoyer: Earley then Boyer)
  --counts times, in suite order. Setup and teardown use the suite's own
  hooks and stay on the elapsed clock; only completed work advances a car.
  Loading and parsing precede the clock. Date timestamps have 1 ms
  resolution on every engine.
- score.js: the suite's own scoring, BenchmarkSuite.RunSuites as run.js
  does it.

Each program's value is its JSON result; the -cli.js copies print it, for a
command-line shell such as QuickJS's qjs.

Usage: make_suite.py <suite dir> <out dir> [--counts 533,213,...]"""
import argparse
import json
from pathlib import Path

FILES = ["base", "richards", "deltablue", "crypto", "raytrace", "earley-boyer", "regexp", "splay", "navier-stokes"]

FIXED = r'''
var __race_result = (function () {
  var counts = COUNTS, ms = [], legs = [], start = +new Date();
  // Each invocation is a fresh process, as in the official suite.
  for (var i = 0; i < BenchmarkSuite.suites.length; i++) {
    var suite = BenchmarkSuite.suites[i], bs = suite.benchmarks;
    var leg = {name:suite.name, count:counts[i], start_ms:+new Date()-start, ms:[]};
    for (var b = 0; b < bs.length; b++) bs[b].Setup();
    leg.setup_end_ms = +new Date()-start;
    for (var j = 0; j < counts[i]; j++) {
      for (var b = 0; b < bs.length; b++) bs[b].run();
      var t = +new Date()-start;
      ms.push(t); leg.ms.push(t);
    }
    for (var b = 0; b < bs.length; b++) bs[b].TearDown();
    leg.end_ms = +new Date()-start;
    legs.push(leg);
  }
  return {kind:"v8-fixed", counts:counts, ms:ms, legs:legs, total_ms:+new Date()-start};
})();
'''

SCORE = r'''
var __race_result = (function () {
  var scores={}, errors={}, start=+new Date();
  BenchmarkSuite.RunSuites({
    NotifyResult:function(name,result){scores[name]=String(result);},
    NotifyError:function(name,error){errors[name]=String(error);},
    NotifyScore:function(score){scores.Score=String(score);}
  });
  return {kind:"v8-score", scores:scores, errors:errors, total_ms:+new Date()-start};
})();
'''


def main():
    p = argparse.ArgumentParser()
    p.add_argument("suite", type=Path)
    p.add_argument("out", type=Path)
    p.add_argument("--counts", default="10,10,10,10,10,10,10,10")
    a = p.parse_args()
    src = "\n".join((a.suite / (f + ".js")).read_text() for f in FILES)
    a.out.mkdir(parents=True, exist_ok=True)
    fixed = FIXED.replace("COUNTS", json.dumps([int(n) for n in a.counts.split(",")]))
    for name, driver in [("fixed", fixed), ("score", SCORE)]:
        (a.out / (name + ".js")).write_text(src + driver + "\nJSON.stringify(__race_result);\n")
        (a.out / (name + "-cli.js")).write_text(
            src + driver + "\n" + 'if(typeof print==="function") print(JSON.stringify(__race_result)); '
            'else console.log(JSON.stringify(__race_result));\n')
    (a.out / "counts.json").write_text(a.counts + "\n")


if __name__ == "__main__":
    main()
