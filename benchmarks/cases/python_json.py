import json


def main() -> None:
    encoded: str = json.dumps([1, 2, 3])
    print(encoded)


main()
