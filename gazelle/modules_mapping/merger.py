import json
import sys


def merge_json(output_file, input_files):
    output_map = {}
    for path in input_files:
        with open(path) as f:
            output_map.update(json.load(f))

    with open(output_file, "w") as f:
        json.dump(output_map, f)

    return 0


if __name__ == "__main__":
    output_file = sys.argv[1]
    input_files = sys.argv[2:]
    exit(merge_json(output_file, input_files))
