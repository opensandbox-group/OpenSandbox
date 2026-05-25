#!/usr/bin/env node
/**
 * Copyright 2025 Alibaba Group Holding Ltd.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */


/**
 * Generate spec-inline.js from sandbox-lifecycle.yml
 *
 * Usage:
 *   node scripts/spec-doc/generate-spec.js
 *   node scripts/spec-doc/generate-spec.js --output docs/public/api/spec-inline.js
 *
 * This script:
 * 1. Reads specs/sandbox-lifecycle.yml
 * 2. Encodes the YAML as a JavaScript string literal
 * 3. Assigns it to the inline spec constant
 * 4. Writes to docs/public/api/spec-inline.js (by default)
 */

const fs = require('fs');
const path = require('path');

// Find project root
function findProjectRoot() {
  let dir = __dirname;
  while (dir !== path.dirname(dir)) {
    if (fs.existsSync(path.join(dir, 'specs', 'sandbox-lifecycle.yml'))) {
      return dir;
    }
    dir = path.dirname(dir);
  }
  throw new Error('Could not find project root (sandbox-lifecycle.yml not found)');
}

function parseOutputPathArg(projectRoot) {
  const outputFlagIndex = process.argv.indexOf('--output');
  if (outputFlagIndex === -1) {
    return path.join(projectRoot, 'docs', 'public', 'api', 'spec-inline.js');
  }
  const outputValue = process.argv[outputFlagIndex + 1];
  if (!outputValue) {
    throw new Error('Missing value for --output');
  }
  if (path.isAbsolute(outputValue)) {
    return outputValue;
  }
  return path.join(projectRoot, outputValue);
}

function main() {
  try {
    const projectRoot = findProjectRoot();
    const yamlPath = path.join(projectRoot, 'specs', 'sandbox-lifecycle.yml');
    const outputPath = parseOutputPathArg(projectRoot);

    // Validate input file exists
    if (!fs.existsSync(yamlPath)) {
      throw new Error(`YAML file not found: ${yamlPath}`);
    }

    console.log('Generating spec-inline.js...');
    console.log(`   Input:  ${yamlPath}`);
    console.log(`   Output: ${outputPath}`);
    fs.mkdirSync(path.dirname(outputPath), { recursive: true });


    // Read YAML
    const yamlContent = fs.readFileSync(yamlPath, 'utf-8');
    const yamlSize = Math.round(yamlContent.length / 1024);

    // Generate JavaScript
    const jsContent = `const OPENAPI_SPEC_YAML = ${JSON.stringify(yamlContent)};`;
    const jsSize = Math.round(jsContent.length / 1024);

    // Write output
    fs.writeFileSync(outputPath, jsContent, 'utf-8');

    console.log('\nSuccessfully generated spec-inline.js');
    console.log(`   YAML size: ${yamlSize} KB`);
    console.log(`   JS size:   ${jsSize} KB`);
    console.log(`   Compression ratio: ${((jsSize / yamlSize) * 100).toFixed(1)}%`);

    // Verify
    const generated = fs.readFileSync(outputPath, 'utf-8');
    if (generated.startsWith('const OPENAPI_SPEC_YAML = ')) {
      console.log('\nFile validated successfully');
      process.exit(0);
    } else {
      throw new Error('Generated file validation failed');
    }
  } catch (error) {
    console.error(`\nError: ${error.message}`);
    console.error(error.stack);
    process.exit(1);
  }
}

// Run
main();
