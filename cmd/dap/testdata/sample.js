// Sample script for the goja DAP smoke test.
var greeting = "hello";

function add(a, b) {
    var sum = a + b;
    return sum;
}

function main() {
    var x = 1;
    var y = 2;
    var z = add(x, y);
    console.log("z =", z);
    var obj = { name: "goja", tags: ["js", "go", "dap"] };
    console.log("obj:", obj.name, obj.tags.length);
    debugger;
    return z * 2;
}

var result = main();
console.log("result =", result);
