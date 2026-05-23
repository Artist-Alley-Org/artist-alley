#!/usr/bin/env node

const fs = require('fs');
const path = require('path');
const filePath = path.join(path.dirname(__dirname), 'include/version.php');

console.log('Bump the cache busting version for ResourceSpace front end dependencies');
try {
    // Reads our version.php file and increment the css_reload_key value (cache busting approach)
    let content = fs.readFileSync(filePath, 'utf8');
    const regex = /\$css_reload_key\s*=\s*(\d+)\s*;/;
    const match = content.match(regex);

    if (!match) {
        throw new Error('$css_reload_key not found in the file.');
    }

    const currentValue = parseInt(match[1], 10);
    const newValue = currentValue + 1;
    const updatedContent = content.replace(regex, `$css_reload_key = ${newValue};`);

    fs.writeFileSync(filePath, updatedContent, 'utf8');

    console.log(`$css_reload_key updated: ${currentValue} → ${newValue}`);
} catch (err) {
    console.error('Error updating $css_reload_key:', err.message);
}